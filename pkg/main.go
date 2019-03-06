package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"

	"time"

	"k8s-plugins/extender-controller-manager/pkg/core"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controller"
)

const (
	MinResyncPeriod = time.Hour * 8
)

var (
	kubeConfig          = flag.String("kubeconfig", "", "kube config file path")
	failPodScaleLimit   = flag.Int("failpodscalelimit", -1, "The pod crash count above the limit the app will be scale down to 0.")
	leaseDuration       = flag.Duration("leader-elect-lease-duration", 15*time.Second, "The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped before it is replaced by another candidate. This is only applicable if leader election is enabled.")
	leaderRetryDuration = flag.Duration("leader-elect-retry-period", 2*time.Second, "The duration the clients should wait between attempting acquisition and renewal of a leadership. This is only applicable if leader election is enabled.")
	leaderRenewDuration = flag.Duration("leader-elect-renew-deadline", 10*time.Second, "The interval between attempts by the acting master to renew a leadership slot before it stops leading. This must be less than or equal to the lease duration. This is only applicable if leader election is enabled.")
	leaderResourceLock  = flag.String("leader-elect-resource-lock", "endpoints", "The type of resource object that is used for locking during leader election. Supported options are endpoints (default) and `configmaps`.")
	leaderElect         = flag.Bool("leader-elect", true, "Start a leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability.")
)

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func getClientset() (*kubernetes.Clientset, error) {
	config, errConfig := buildConfig(*kubeConfig)
	if errConfig != nil {
		return nil, errConfig
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

func ResyncPeriod() func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1
		return time.Duration(float64(MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// WaitForAPIServer waits for the API Server's /healthz endpoint to report "ok" with timeout.
func WaitForAPIServer(client kubernetes.Interface, timeout time.Duration) error {
	var lastErr error

	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		healthStatus := 0
		result := client.Discovery().RESTClient().Get().AbsPath("/healthz").Do().StatusCode(&healthStatus)
		if result.Error() != nil {
			lastErr = fmt.Errorf("failed to get apiserver /healthz status: %v", result.Error())
			return false, nil
		}
		if healthStatus != http.StatusOK {
			content, _ := result.Raw()
			lastErr = fmt.Errorf("APIServer isn't healthy: %v", string(content))
			glog.Warningf("APIServer isn't healthy yet: %v. Waiting a little while.", string(content))
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		return fmt.Errorf("%v: %v", err, lastErr)
	}

	return nil
}

func CreateControllerContext(stop <-chan struct{}) (core.ControllerContext, error) {
	config, err := buildConfig(*kubeConfig)
	if err != nil {
		return core.ControllerContext{}, err
	}
	rootClientBuilder := controller.SimpleControllerClientBuilder{
		ClientConfig: config,
	}

	versionedClient := rootClientBuilder.ClientOrDie("shared-informers")
	sharedInformers := informers.NewSharedInformerFactory(versionedClient, ResyncPeriod()())

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := WaitForAPIServer(versionedClient, 10*time.Second); err != nil {
		return core.ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %v", err)
	}

	// Use a discovery client capable of being refreshed.
	discoveryClient := rootClientBuilder.ClientOrDie("controller-discovery")
	cachedClient := cacheddiscovery.NewMemCacheClient(discoveryClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
	go wait.Until(func() {
		restMapper.Reset()
	}, 30*time.Second, stop)

	ctx := core.ControllerContext{
		ClientBuilder:   rootClientBuilder,
		InformerFactory: sharedInformers,
		ComponentConfig: core.EnndataControllerManagerConfiguration{
			FailPodScaleLimit: int32(*failPodScaleLimit),
		},
		RESTMapper: restMapper,
		Stop:       stop,
	}

	return ctx, nil
}

func createRecorder(kubeClient kubernetes.Interface, userAgent string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return eventBroadcaster.NewRecorder(legacyscheme.Scheme, v1.EventSource{Component: userAgent})
}

func main() {
	flag.Parse()
	defer glog.Flush()

	stopCh := make(chan struct{})
	run := func(stop <-chan struct{}) {
		ctx, err := CreateControllerContext(stopCh)
		if err != nil {
			glog.Errorf("CreateControllerContext err:%v", err)
			os.Exit(1)
		}
		controllersMap := core.NewControllerInitializers()
		for name, controller := range controllersMap {
			glog.Infof("start controller %s", name)
			if ok, err := controller(ctx); err != nil {
				glog.Errorf("start controller %s ok:%t, err:%v", name, ok, err)
				os.Exit(2)
			} else if ok == false {
				glog.Infof("skip controller %s", name)
			}
		}
		ctx.InformerFactory.Start(ctx.Stop)
		select {}
	}
	if !*leaderElect {
		run(wait.NeverStop)
		panic("unreachable")
	}
	id, err := os.Hostname()
	if err != nil {
		glog.Errorf("get hostname err:%v", err)
		os.Exit(3)
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())
	client, errClient := getClientset()
	if errClient != nil {
		glog.Errorf("getClientset err:%v", errClient)
		os.Exit(4)
	}
	rl, err := resourcelock.New(*leaderResourceLock,
		"k8splugin",
		"enndata-controller-manager",
		client.CoreV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: createRecorder(client, "enndata-controller-manager-elect"),
		})
	if err != nil {
		glog.Fatalf("error creating lock: %v", err)
	}

	leaderelection.RunOrDie(leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: *leaseDuration,
		RenewDeadline: *leaderRenewDuration,
		RetryPeriod:   *leaderRetryDuration,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				glog.Fatalf("leaderelection lost")
			},
		},
	})
	panic("unreachable")
}
