/*
Copyright 2019 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ssh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"

	"golang.org/x/crypto/ssh"

	"k8s.io/api/core/v1"
	"k8s.io/klog"

	batch "volcano.sh/volcano/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/apis/helpers"
	jobhelpers "volcano.sh/volcano/pkg/controllers/job/helpers"
	"volcano.sh/volcano/pkg/controllers/job/plugins/env"
	"volcano.sh/volcano/pkg/controllers/job/plugins/interface"
)

type sshPlugin struct {
	// Arguments given for the plugin
	pluginArguments []string

	Clientset pluginsinterface.PluginClientset

	// flag parse args
	noRoot         bool
	sshKeyFilePath string
}

// New creates ssh plugin
func New(client pluginsinterface.PluginClientset, arguments []string) pluginsinterface.PluginInterface {
	sshPlugin := sshPlugin{
		pluginArguments: arguments,
		Clientset:       client,
		sshKeyFilePath:  SSHAbsolutePath,
	}

	sshPlugin.addFlags()
	// if not set ssh key files path, use the default.
	if sshPlugin.noRoot && sshPlugin.sshKeyFilePath == SSHAbsolutePath {
		sshPlugin.sshKeyFilePath = env.ConfigMapMountPath + "/" + SSHRelativePath
	}

	return &sshPlugin
}

func (sp *sshPlugin) Name() string {
	return "ssh"
}

func (sp *sshPlugin) OnPodCreate(pod *v1.Pod, job *batch.Job) error {
	sp.mountRsaKey(pod, job)

	return nil
}

func (sp *sshPlugin) OnJobAdd(job *batch.Job) error {
	if job.Status.ControlledResources["plugin-"+sp.Name()] == sp.Name() {
		return nil
	}

	data, err := generateRsaKey(job)
	if err != nil {
		return err
	}

	if err := helpers.CreateSecret(job, sp.Clientset.KubeClients, data, sp.secretName(job)); err != nil {
		return fmt.Errorf("create secret for job <%s/%s> with ssh plugin failed for %v",
			job.Namespace, job.Name, err)
	}

	job.Status.ControlledResources["plugin-"+sp.Name()] = sp.Name()

	return nil
}

func (sp *sshPlugin) OnJobDelete(job *batch.Job) error {
	return helpers.DeleteSecret(job, sp.Clientset.KubeClients, sp.secretName(job))
}

func (sp *sshPlugin) mountRsaKey(pod *v1.Pod, job *batch.Job) {
	secretName := sp.secretName(job)

	sshVolume := v1.Volume{
		Name: secretName,
	}

	var mode int32 = 0600
	sshVolume.Secret = &v1.SecretVolumeSource{
		SecretName: secretName,
		Items: []v1.KeyToPath{
			{
				Key:  SSHPrivateKey,
				Path: SSHRelativePath + "/" + SSHPrivateKey,
			},
			{
				Key:  SSHPublicKey,
				Path: SSHRelativePath + "/" + SSHPublicKey,
			},
			{
				Key:  SSHPublicKey,
				Path: SSHRelativePath + "/" + SSHAuthorizedKeys,
			},
			{
				Key:  SSHConfig,
				Path: SSHRelativePath + "/" + SSHConfig,
			},
		},
		DefaultMode: &mode,
	}

	if sp.sshKeyFilePath != SSHAbsolutePath {
		var noRootMode int32 = 0755
		sshVolume.Secret.DefaultMode = &noRootMode
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, sshVolume)

	for i, c := range pod.Spec.Containers {
		vm := v1.VolumeMount{
			MountPath: sp.sshKeyFilePath,
			SubPath:   SSHRelativePath,
			Name:      secretName,
		}

		pod.Spec.Containers[i].VolumeMounts = append(c.VolumeMounts, vm)
	}

	return
}

func generateRsaKey(job *batch.Job) (map[string][]byte, error) {
	bitSize := 1024

	privateKey, err := rsa.GenerateKey(rand.Reader, bitSize)
	if err != nil {
		klog.Errorf("rsa generateKey err: %v", err)
		return nil, err
	}

	// id_rsa
	privBlock := pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privateKeyBytes := pem.EncodeToMemory(&privBlock)

	// id_rsa.pub
	publicRsaKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		klog.Errorf("ssh newPublicKey err: %v", err)
		return nil, err
	}
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicRsaKey)

	data := make(map[string][]byte)
	data[SSHPrivateKey] = privateKeyBytes
	data[SSHPublicKey] = publicKeyBytes
	data[SSHConfig] = []byte(generateSSHConfig(job))

	return data, nil
}

func (sp *sshPlugin) secretName(job *batch.Job) string {
	return fmt.Sprintf("%s-%s-%s", job.Name, job.UID, sp.Name())
}

func (sp *sshPlugin) addFlags() {
	flagSet := flag.NewFlagSet(sp.Name(), flag.ContinueOnError)
	// TODO: deprecate no-root
	flagSet.BoolVar(&sp.noRoot, "no-root", sp.noRoot, "The ssh user, --no-root is common user")
	flagSet.StringVar(&sp.sshKeyFilePath, "ssh-key-file-path", sp.sshKeyFilePath, "The path used to store "+
		"ssh private and public keys, it is `/root/.ssh` by default.")

	if err := flagSet.Parse(sp.pluginArguments); err != nil {
		klog.Errorf("plugin %s flagset parse failed, err: %v", sp.Name(), err)
	}
	return
}

func generateSSHConfig(job *batch.Job) string {
	config := "StrictHostKeyChecking no\nUserKnownHostsFile /dev/null\n"

	for _, ts := range job.Spec.Tasks {
		for i := 0; i < int(ts.Replicas); i++ {
			hostName := ts.Template.Spec.Hostname
			subdomain := ts.Template.Spec.Subdomain
			if len(hostName) == 0 {
				hostName = jobhelpers.MakePodName(job.Name, ts.Name, i)
			}
			if len(subdomain) == 0 {
				subdomain = job.Name
			}

			config += "Host " + hostName + "\n"
			config += "  HostName " + hostName + "." + subdomain + "\n"
			if len(ts.Template.Spec.Hostname) != 0 {
				break
			}
		}
	}

	return config
}
