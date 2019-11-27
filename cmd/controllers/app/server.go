/*
Copyright 2017 The Vulcan Authors.

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

package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	// Initialize client auth plugin.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"volcano.sh/volcano/cmd/controllers/app/options"
	"volcano.sh/volcano/pkg/apis/helpers"
	vcclientset "volcano.sh/volcano/pkg/client/clientset/versioned"
	"volcano.sh/volcano/pkg/controllers/garbagecollector"
	"volcano.sh/volcano/pkg/controllers/job"
	"volcano.sh/volcano/pkg/controllers/podgroup"
	"volcano.sh/volcano/pkg/controllers/queue"
)

const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 5 * time.Second
)

func buildConfig(opt *options.ServerOption) (*rest.Config, error) {
	var cfg *rest.Config
	var err error

	master := opt.Master
	kubeconfig := opt.Kubeconfig
	if master != "" || kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags(master, kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	cfg.QPS = opt.KubeAPIQPS
	cfg.Burst = opt.KubeAPIBurst

	return cfg, nil
}

//Run the controller
func Run(opt *options.ServerOption) error {
	config, err := buildConfig(opt)
	if err != nil {
		return err
	}

	if err := helpers.StartHealthz(opt.HealthzBindAddress, "volcano-controller"); err != nil {
		return err
	}

	run := startControllers(config, opt)

	if !opt.EnableLeaderElection {
		run(context.TODO())
		return fmt.Errorf("finished without leader elect")
	}

	leaderElectionClient, err := kubeclientset.NewForConfig(rest.AddUserAgent(config, "leader-election"))
	if err != nil {
		return err
	}

	// Prepare event clients.
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: leaderElectionClient.CoreV1().Events(opt.LockObjectNamespace)})
	eventRecorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "vc-controllers"})

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("unable to get hostname: %v", err)
	}
	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id := hostname + "_" + string(uuid.NewUUID())

	rl, err := resourcelock.New(resourcelock.ConfigMapsResourceLock,
		opt.LockObjectNamespace,
		"vc-controllers",
		leaderElectionClient.CoreV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: eventRecorder,
		})
	if err != nil {
		return fmt.Errorf("couldn't create resource lock: %v", err)
	}

	leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				klog.Fatalf("leaderelection lost")
			},
		},
	})
	return fmt.Errorf("lost lease")
}

func startControllers(config *rest.Config, opt *options.ServerOption) func(ctx context.Context) {
	// TODO: add user agent for different controllers
	kubeClient := kubeclientset.NewForConfigOrDie(config)
	vcClient := vcclientset.NewForConfigOrDie(config)

	sharedInformers := informers.NewSharedInformerFactory(kubeClient, 0)

	jobController := job.NewJobController(kubeClient, vcClient, sharedInformers, opt.WorkerThreads)
	queueController := queue.NewQueueController(kubeClient, vcClient)
	garbageCollector := garbagecollector.NewGarbageCollector(vcClient)
	pgController := podgroup.NewPodgroupController(kubeClient, vcClient, sharedInformers, opt.SchedulerName)

	return func(ctx context.Context) {
		go jobController.Run(ctx.Done())
		go queueController.Run(ctx.Done())
		go garbageCollector.Run(ctx.Done())
		go pgController.Run(ctx.Done())
		<-ctx.Done()
	}
}
