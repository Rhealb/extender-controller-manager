package core

import (
	"github.com/Rhealb/extender-controller-manager/pkg/failpodscaler"
	"github.com/Rhealb/extender-controller-manager/pkg/hostpathpv"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/kubernetes/pkg/controller"
)

type InitFunc func(ctx ControllerContext) (bool, error)

type EnndataControllerManagerConfiguration struct {
	FailPodScaleLimit int32
}

type ControllerContext struct {
	ClientBuilder controller.ControllerClientBuilder

	InformerFactory informers.SharedInformerFactory

	ComponentConfig EnndataControllerManagerConfiguration

	Stop <-chan struct{}

	RESTMapper *restmapper.DeferredDiscoveryRESTMapper
	//	InformersStarted chan struct{}

	//	ResyncPeriod func() time.Duration
}

func NewControllerInitializers() map[string]InitFunc {
	controllers := map[string]InitFunc{}
	controllers["nodepvcontroller"] = startNodePVController
	controllers["failpodscalercontroller"] = startFailPodScalerController
	return controllers
}

func startNodePVController(ctx ControllerContext) (bool, error) {
	go hostpathpv.NewNodePVController(
		ctx.InformerFactory.Core().V1().Nodes(),
		ctx.InformerFactory.Core().V1().PersistentVolumes(),
		ctx.ClientBuilder.ClientOrDie("nodepv-controller"),
		ctx.Stop,
	).Run(ctx.Stop)
	return true, nil
}

func startFailPodScalerController(ctx ControllerContext) (bool, error) {
	if ctx.ComponentConfig.FailPodScaleLimit <= 0 { // if FailPodScaleLimit <=0 FailPodScalerController will be disabled
		return true, nil
	}
	fpsClientGoClient := ctx.ClientBuilder.ClientOrDie("fail-pod-scaler")
	fpsClientConfig := ctx.ClientBuilder.ConfigOrDie("fail-pod-scaler")

	// we don't use cached discovery because DiscoveryScaleKindResolver does its own caching,
	// so we want to re-fetch every time when we actually ask for it
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(fpsClientGoClient.Discovery())
	scaleClient, err := scale.NewForConfig(fpsClientConfig, ctx.RESTMapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
	if err != nil {
		return false, err
	}
	go failpodscaler.NewFailPodScalerController(
		fpsClientGoClient.CoreV1(),
		scaleClient,
		ctx.RESTMapper,
		ctx.InformerFactory.Core().V1().Pods(),
		ctx.InformerFactory.Apps().V1().ReplicaSets(),
		fpsClientGoClient,
		ctx.ComponentConfig.FailPodScaleLimit,
	).Run(ctx.Stop)
	return true, nil
}
