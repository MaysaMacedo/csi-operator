package operator

import (
	"context"
	"path/filepath"
	"time"

	"github.com/openshift/csi-operator/assets"
	"github.com/openshift/csi-operator/pkg/clients"
	"github.com/openshift/csi-operator/pkg/driver/common/operator"
	generated_assets "github.com/openshift/csi-operator/pkg/generated-assets"
	"github.com/openshift/csi-operator/pkg/generator"
	"github.com/openshift/csi-operator/pkg/operator/config"
	"github.com/openshift/csi-operator/pkg/operator/volume_snapshot_class"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
)

type ConfigProvider func(flavour generator.ClusterFlavour, c *clients.Clients) *config.OperatorConfig

const (
	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext, guestKubeConfigString string, opConfig *config.OperatorConfig) error {
	klog.V(2).Infof("Running openshift/csi-operator for %s", opConfig.CSIDriverName)
	isHypershift := guestKubeConfigString != ""
	controlPlaneNamespace := controllerConfig.OperatorNamespace

	flavour := generator.FlavourStandalone
	if isHypershift {
		flavour = generator.FlavourHyperShift
	}

	// Create Clients
	builder := clients.NewBuilder(opConfig.UserAgent, string(opConfig.CSIDriverName), controllerConfig, resync).
		WithHyperShiftGuest(guestKubeConfigString, opConfig.CloudConfigNamespace)

	c := builder.BuildOrDie(ctx)

	klog.Infof("Building clients is done")

	// Build ControllerConfig
	csiOperatorControllerConfig, err := opConfig.OperatorControllerConfigBuilder(ctx, flavour, c)
	if err != nil {
		klog.Errorf("error building operator config: %v", err)
		return err
	}

	// Load generated assets.
	assetDir := filepath.Join(opConfig.AssetDir, string(flavour))
	a, err := generated_assets.NewFromAssets(assets.ReadFile, assetDir)
	if err != nil {
		return err
	}
	defaultReplacements := operator.DefaultReplacements(controlPlaneNamespace)
	if csiOperatorControllerConfig.ExtraReplacementsFunc != nil {
		defaultReplacements = append(defaultReplacements, csiOperatorControllerConfig.ExtraReplacementsFunc()...)
	}

	a.SetReplacements(defaultReplacements)

	// Start controllers that manage resources in the MANAGEMENT cluster.
	controlPlaneControllerInformers := csiOperatorControllerConfig.DeploymentInformers
	controllerHooks := csiOperatorControllerConfig.DeploymentHooks

	if len(csiOperatorControllerConfig.DeploymentWatchedSecretNames) > 0 {
		controlPlaneSecretInformer := c.GetControlPlaneSecretInformer(controlPlaneNamespace)
		for _, secretName := range csiOperatorControllerConfig.DeploymentWatchedSecretNames {
			controllerHooks = append(controllerHooks, csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(controlPlaneNamespace, secretName, controlPlaneSecretInformer))
		}
		controlPlaneControllerInformers = append(controlPlaneControllerInformers, controlPlaneSecretInformer.Informer())
	}

	controlPlaneCSIControllerSet := csicontrollerset.NewCSIControllerSet(
		c.OperatorClient,
		c.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		csiOperatorControllerConfig.GetControllerName("CSIDriver"),
		false,
	).WithStaticResourcesController(
		csiOperatorControllerConfig.GetControllerName("DriverControlPlaneStaticResourcesController"),
		c.ControlPlaneKubeClient,
		c.ControlPlaneDynamicClient,
		c.ControlPlaneKubeInformers,
		a.GetAsset,
		a.GetControllerStaticAssetNames(),
	).WithCSIConfigObserverController(
		csiOperatorControllerConfig.GetControllerName("DriverCSIConfigObserverController"),
		c.ConfigInformers,
	).WithCSIDriverControllerService(
		csiOperatorControllerConfig.GetControllerName("DriverControllerServiceController"),
		a.GetAsset,
		generated_assets.ControllerDeploymentAssetName,
		c.ControlPlaneKubeClient,
		c.ControlPlaneKubeInformers.InformersFor(controlPlaneNamespace),
		c.ConfigInformers,
		controlPlaneControllerInformers,
		controllerHooks...,
	)
	if err != nil {
		return err
	}

	guestDaemonSetHooks := csiOperatorControllerConfig.GuestDaemonSetHooks
	guestDaemonInformers := csiOperatorControllerConfig.GuestDaemonSetInformers

	if len(csiOperatorControllerConfig.DaemonSetWatchedSecretNames) > 0 {
		nodeSecretInformer := c.GetNodeSecretInformer(clients.CSIDriverNamespace)
		for _, secretName := range csiOperatorControllerConfig.DaemonSetWatchedSecretNames {
			guestDaemonSetHooks = append(guestDaemonSetHooks, csidrivernodeservicecontroller.WithSecretHashAnnotationHook(clients.CSIDriverNamespace, secretName, nodeSecretInformer))
			guestDaemonInformers = append(guestDaemonInformers, nodeSecretInformer.Informer())
		}
	}

	// Prepare controllers that manage resources in the GUEST cluster.
	guestCSIControllerSet := csicontrollerset.NewCSIControllerSet(
		c.OperatorClient,
		c.EventRecorder,
	).WithStaticResourcesController(
		csiOperatorControllerConfig.GetControllerName("DriverGuestStaticResourcesController"),
		c.KubeClient,
		c.DynamicClient,
		c.KubeInformers,
		a.GetAsset,
		a.GetGuestStaticAssetNames(),
	).WithCSIDriverNodeService(
		csiOperatorControllerConfig.GetControllerName("DriverNodeServiceController"),
		a.GetAsset,
		generated_assets.NodeDaemonSetAssetName,
		c.KubeClient,
		c.KubeInformers.InformersFor(clients.CSIDriverNamespace),
		guestDaemonInformers,
		guestDaemonSetHooks...,
	)

	// Prepare StorageClassController when needed
	if scNames := a.GetStorageClassAssetNames(); len(scNames) > 0 {
		guestCSIControllerSet = guestCSIControllerSet.WithStorageClassController(
			csiOperatorControllerConfig.GetControllerName("DriverStorageClassController"),
			a.GetAsset,
			scNames,
			c.KubeClient,
			c.KubeInformers.InformersFor(""),
			c.OperatorInformers,
			// TODO: add extra informers
			csiOperatorControllerConfig.StorageClassHooks...,
		)
	}

	snapshotAssetNames := a.GetVolumeSnapshotClassAssetNames()

	// Prepare static resource controller for VolumeSnapshotClasses when needed
	if len(snapshotAssetNames) > 0 {
		snapshotClassController := volume_snapshot_class.NewVolumeSnapshotClassController(
			csiOperatorControllerConfig.GetControllerName("VolumeSnapshotController"),
			a.GetAsset,
			snapshotAssetNames,
			builder,
			c.EventRecorder,
			csiOperatorControllerConfig.VolumeSnapshotClassHooks...,
		)
		csiOperatorControllerConfig.ExtraControlPlaneControllers = append(csiOperatorControllerConfig.ExtraControlPlaneControllers, snapshotClassController)
	}

	// Start all informers
	c.Start(ctx)
	klog.V(2).Infof("Waiting for informers to sync")
	c.WaitForCacheSync(ctx)
	klog.V(2).Infof("Informers synced")

	// Start controllers
	for _, controller := range csiOperatorControllerConfig.ExtraControlPlaneControllers {
		klog.Infof("Starting controller %s", controller.Name())
		go controller.Run(ctx, 1)
	}
	klog.Info("Starting control plane controllerset")
	go controlPlaneCSIControllerSet.Run(ctx, 1)
	klog.Info("Starting guest controllerset")
	go guestCSIControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return nil
}
