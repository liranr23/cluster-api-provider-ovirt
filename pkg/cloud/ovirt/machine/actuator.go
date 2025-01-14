/*
Copyright oVirt Authors
SPDX-License-Identifier: Apache-2.0
*/

package machine

import (
	"context"
	"fmt"
	"k8s.io/client-go/rest"
	"time"

	machinev1 "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	apierrors "github.com/openshift/machine-api-operator/pkg/controller/machine"
	"github.com/openshift/machine-api-operator/pkg/generated/clientset/versioned/typed/machine/v1beta1"
	"github.com/openshift/machine-api-operator/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	osclientset "github.com/openshift/client-go/config/clientset/versioned"
	ovirtconfigv1 "github.com/openshift/cluster-api-provider-ovirt/pkg/apis/ovirtprovider/v1beta1"
	"github.com/openshift/cluster-api-provider-ovirt/pkg/cloud/ovirt"
	"github.com/openshift/cluster-api-provider-ovirt/pkg/cloud/ovirt/clients"
	ovirtsdk "github.com/ovirt/go-ovirt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	TimeoutInstanceCreate       = 5 * time.Minute
	RetryIntervalInstanceStatus = 10 * time.Second
	InstanceStatusAnnotationKey = "machine.openshift.io/instance-state"
)

type OvirtActuator struct {
	params         ovirt.ActuatorParams
	scheme         *runtime.Scheme
	client         client.Client
	KubeClient     *kubernetes.Clientset
	machinesClient v1beta1.MachineV1beta1Interface
	EventRecorder  record.EventRecorder
	ovirtApi       *ovirtsdk.Connection
	OSClient       osclientset.Interface
}



func NewActuator(params ovirt.ActuatorParams) (*OvirtActuator, error) {
	config := ctrl.GetConfigOrDie()
	osClient := osclientset.NewForConfigOrDie(rest.AddUserAgent(config, "cluster-api-provider-ovirt"))

	return &OvirtActuator{
		params:         params,
		client:         params.Client,
		machinesClient: params.MachinesClient,
		scheme:         params.Scheme,
		KubeClient:     params.KubeClient,
		EventRecorder:  params.EventRecorder,
		ovirtApi:       nil,
		OSClient:       osClient,
	}, nil
}

func (actuator *OvirtActuator) Create(ctx context.Context, machine *machinev1.Machine) error {
	providerSpec, err := ovirtconfigv1.ProviderSpecFromRawExtension(machine.Spec.ProviderSpec.Value)
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err))
	}

	connection, err := actuator.getConnection(machine.Namespace, providerSpec.CredentialsSecret.Name)
	if err != nil {
		return fmt.Errorf("failed to create connection to oVirt API")
	}

	machineService, err := clients.NewInstanceServiceFromMachine(machine, connection)
	if err != nil {
		return err
	}

	if verr := actuator.validateMachine(machine, providerSpec); verr != nil {
		return actuator.handleMachineError(machine, verr)
	}

	// creating a new instance, we don't have the vm id yet
	instance, err := machineService.GetVmByName()
	if err != nil {
		return err
	}
	if instance != nil {
		klog.Infof("Skipped creating a VM that already exists.\n")
		return nil
	}

	instance, err = machineService.InstanceCreate(machine, providerSpec, actuator.KubeClient)
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.CreateMachine(
			"error creating Ovirt instance: %v", err))
	}

	// Wait till ready
	err = util.PollImmediate(RetryIntervalInstanceStatus, TimeoutInstanceCreate, func() (bool, error) {
		instance, err := machineService.GetVm(*machine)
		if err != nil {
			return false, nil
		}
		return instance.MustStatus() == ovirtsdk.VMSTATUS_DOWN, nil
	})
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.CreateMachine(
			"Error creating oVirt VM: %v", err))
	}

	vmService := machineService.Connection.SystemService().VmsService().VmService(instance.MustId())
	_, err = vmService.Start().Send()
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.CreateMachine(
			"Error running oVirt VM: %v", err))
	}

	// Wait till running
	err = util.PollImmediate(RetryIntervalInstanceStatus, TimeoutInstanceCreate, func() (bool, error) {
		instance, err := machineService.GetVm(*machine)
		if err != nil {
			return false, nil
		}
		return instance.MustStatus() == ovirtsdk.VMSTATUS_UP, nil
	})
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.CreateMachine(
			"Error running oVirt VM: %v", err))
	}

	actuator.EventRecorder.Eventf(machine, corev1.EventTypeNormal, "Created", "Updated Machine %v", machine.Name)
	return actuator.patchMachine(ctx,machine, instance, conditionSuccess())
}

func (actuator *OvirtActuator) Exists(_ context.Context, machine *machinev1.Machine) (bool, error) {
	klog.Infof("Checking machine %v exists.\n", machine.Name)
	providerSpec, err := ovirtconfigv1.ProviderSpecFromRawExtension(machine.Spec.ProviderSpec.Value)
	if err != nil {
		return false, actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err))
	}

	connection, err := actuator.getConnection(machine.Namespace, providerSpec.CredentialsSecret.Name)
	if err != nil {
		return false, fmt.Errorf("failed to create connection to oVirt API")
	}

	machineService, err := clients.NewInstanceServiceFromMachine(machine, connection)
	if err != nil {
		return false, err
	}
	vm, err := machineService.GetVm(*machine)
	if err != nil {
		return false, err
	}
	return vm != nil, err
}

func (actuator *OvirtActuator) Update(ctx context.Context, machine *machinev1.Machine) error {
	// eager update
	providerSpec, err := ovirtconfigv1.ProviderSpecFromRawExtension(machine.Spec.ProviderSpec.Value)
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err))
	}

	connection, err := actuator.getConnection(machine.Namespace, providerSpec.CredentialsSecret.Name)
	if err != nil {
		return fmt.Errorf("failed to create connection to oVirt API")
	}

	machineService, err := clients.NewInstanceServiceFromMachine(machine, connection)
	if err != nil {
		return err
	}

	var vm *clients.Instance
	if machine.Spec.ProviderID == nil || *machine.Spec.ProviderID == "" {
		vm, err = machineService.GetVmByName()
		if err != nil {
			return actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
				"Cannot find a VM by name: %v", err))
		}
	} else {
		vm, err = machineService.GetVm(*machine)
		if err != nil {
			return actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
				"Cannot find a VM by id: %v", err))
		}
	}
	return actuator.patchMachine(ctx,machine, vm, conditionSuccess())
}

func (actuator *OvirtActuator) Delete(_ context.Context, machine *machinev1.Machine) error {
	providerSpec, err := ovirtconfigv1.ProviderSpecFromRawExtension(machine.Spec.ProviderSpec.Value)
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err))
	}
	connection, err := actuator.getConnection(machine.Namespace, providerSpec.CredentialsSecret.Name)
	if err != nil {
		return err
	}

	machineService, err := clients.NewInstanceServiceFromMachine(machine, connection)
	if err != nil {
		return err
	}

	instance, err := machineService.GetVm(*machine)
	if err != nil {
		return err
	}

	if instance == nil {
		klog.Infof("Skipped deleting a VM that is already deleted.\n")
		return nil
	}

	err = machineService.InstanceDelete(instance.MustId())
	if err != nil {
		return actuator.handleMachineError(machine, apierrors.DeleteMachine(
			"error deleting Ovirt instance: %v", err))
	}

	actuator.EventRecorder.Eventf(machine, corev1.EventTypeNormal, "Deleted", "Deleted Machine %v", machine.Name)
	return nil
}

// If the OvirtActuator has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (actuator *OvirtActuator) handleMachineError(machine *machinev1.Machine, err *apierrors.MachineError) error {
	if actuator.client != nil {
		machine.Status.ErrorReason = &err.Reason
		machine.Status.ErrorMessage = &err.Message
		if err := actuator.client.Update(context.TODO(), machine); err != nil {
			return fmt.Errorf("unable to update machine status: %v", err)
		}
	}

	klog.Errorf("Machine error %s: %v", machine.Name, err.Message)
	return err
}

func (actuator *OvirtActuator) patchMachine(ctx context.Context,machine *machinev1.Machine, instance *clients.Instance, condition ovirtconfigv1.OvirtMachineProviderCondition) error {
	actuator.reconcileProviderID(machine, instance)
	klog.V(5).Infof("Machine %s provider status %s", instance.MustName(), instance.MustStatus())

	err := actuator.reconcileNetwork(ctx,machine, instance)
	if err != nil {
		return err
	}
	actuator.reconcileAnnotations(machine, instance)
	err = actuator.reconcileProviderStatus(machine, instance, condition)
	if err != nil {
		return err
	}

	// Copy the status, because its discarded and returned fresh from the DB by the machine resource update.
	// Save it for the status sub-resource update.
	statusCopy := *machine.Status.DeepCopy()
	klog.Info("Updating machine resource")

	// TODO the namespace should be set on actuator creation. Remove the hardcoded openshift-machine-api.
	newMachine, err := actuator.machinesClient.Machines("openshift-machine-api").Update(context.TODO(), machine, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	newMachine.Status = statusCopy
	klog.Info("Updating machine status sub-resource")
	if _, err := actuator.machinesClient.Machines("openshift-machine-api").UpdateStatus(context.TODO(), newMachine, metav1.UpdateOptions{}); err != nil {
		return err
	}
	actuator.EventRecorder.Eventf(newMachine, corev1.EventTypeNormal, "Update", "Updated Machine %v", newMachine.Name)
	return nil
}

func (actuator *OvirtActuator) getClusterAddress(ctx context.Context) (map[string]int,error){
		infra,err := actuator.OSClient.ConfigV1().Infrastructures().Get(ctx,"cluster",metav1.GetOptions{})
		if err != nil {
			klog.Error(err, "Failed to retrieve Cluster details")
			return nil,err
		}

		var clusterAddr = make(map[string]int)
		clusterAddr[ infra.Status.PlatformStatus.Ovirt.APIServerInternalIP ] = 1
		clusterAddr[ infra.Status.PlatformStatus.Ovirt.IngressIP ] = 1

		return clusterAddr,nil
	}

func (actuator *OvirtActuator) reconcileNetwork(ctx context.Context,machine *machinev1.Machine, instance *clients.Instance) error {
	switch instance.MustStatus() {
	// expect IP addresses only on those statuses.
	// in those statuses we 'll try reconciling
	case ovirtsdk.VMSTATUS_UP, ovirtsdk.VMSTATUS_MIGRATING:
		break

	// update machine status.
	case ovirtsdk.VMSTATUS_DOWN:
		return nil

	// return error if vm is transient state this will force retry reconciling until VM is up.
	// there is no event generated that will trigger this.  BZ1854787
	default:
		return fmt.Errorf("Aborting reconciliation while VM %s  state is %s", instance.MustName(), instance.MustStatus())

	}
	name := instance.MustName()
	addresses := []corev1.NodeAddress{{Address: name, Type: corev1.NodeInternalDNS}}
	machineService, err := clients.NewInstanceServiceFromMachine(machine, actuator.ovirtApi)
	if err != nil {
		return err
	}
	vmId := instance.MustId()
	klog.V(5).Infof("using oVirt SDK to find % IP addresses", name)

	//get API and ingress addresses that will be excluded from the node address selection
	excludeAddr, err := actuator.getClusterAddress(ctx)
	if err != nil {
		return err
	}

	ip, err := machineService.FindVirtualMachineIP(vmId,excludeAddr)

	if err != nil {
		// stop reconciliation till we get IP addresses - otherwise the state will be considered stable.
		klog.Errorf("failed to lookup the VM IP %s - skip setting addresses for this machine", err)
		return err
	} else {
		klog.V(5).Infof("received IP address %v from engine", ip)
		addresses = append(addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	machine.Status.Addresses = addresses
	return nil
}

func (actuator *OvirtActuator) reconcileProviderStatus(machine *machinev1.Machine, instance *clients.Instance, condition ovirtconfigv1.OvirtMachineProviderCondition) error {
	status := string(instance.MustStatus())
	name := instance.MustId()

	providerStatus, err := ovirtconfigv1.ProviderStatusFromRawExtension(machine.Status.ProviderStatus)
	if err != nil {
		return err
	}
	providerStatus.InstanceState = &status
	providerStatus.InstanceID = &name
	providerStatus.Conditions = actuator.reconcileConditions(providerStatus.Conditions, condition)
	rawExtension, err := ovirtconfigv1.RawExtensionFromProviderStatus(providerStatus)
	if err != nil {
		return err
	}
	machine.Status.ProviderStatus = rawExtension
	return nil
}

func (actuator *OvirtActuator) reconcileProviderID(machine *machinev1.Machine, instance *clients.Instance) {
	id := instance.MustId()
	providerID := ovirt.ProviderIDPrefix + id
	machine.Spec.ProviderID = &providerID

	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[ovirt.OvirtIdAnnotationKey] = id
}

func (actuator *OvirtActuator) reconcileConditions(
	conditions []ovirtconfigv1.OvirtMachineProviderCondition,
	newCondition ovirtconfigv1.OvirtMachineProviderCondition) []ovirtconfigv1.OvirtMachineProviderCondition {

	if conditions == nil {
		now := metav1.Now()
		newCondition.LastProbeTime = now
		newCondition.LastTransitionTime = now
		return []ovirtconfigv1.OvirtMachineProviderCondition{newCondition}
	}

	for _, c := range conditions {
		if c.Type == newCondition.Type {
			if c.Reason != newCondition.Reason || c.Message != newCondition.Message {
				if c.Status != newCondition.Status {
					newCondition.LastTransitionTime = metav1.Now()
				}
				c.Status = newCondition.Status
				c.Message = newCondition.Message
				c.Reason = newCondition.Reason
				c.LastProbeTime = newCondition.LastProbeTime
				return conditions
			}
		}
	}
	return conditions
}

func (actuator *OvirtActuator) validateMachine(machine *machinev1.Machine, config *ovirtconfigv1.OvirtMachineProviderSpec) *apierrors.MachineError {
	return nil
}

//createApiConnection returns a a client to oVirt's API endpoint
func createApiConnection(client client.Client, namespace string, secretName string) (*ovirtsdk.Connection, error) {
	creds, err := clients.GetCredentialsSecret(client, namespace, secretName)

	if err != nil {
		klog.Infof("failed getting credentials for namespace %s, %s", namespace, err)
		return nil, err
	}

	connection, err := ovirtsdk.NewConnectionBuilder().
		URL(creds.URL).
		Username(creds.Username).
		Password(creds.Password).
		CAFile(creds.CAFile).
		Insecure(creds.Insecure).
		Build()
	if err != nil {
		return nil, err
	}

	return connection, nil
}

//getConnection returns a a client to oVirt's API endpoint
func (actuator *OvirtActuator) getConnection(namespace, secretName string) (*ovirtsdk.Connection, error) {
	var err error
	if actuator.ovirtApi == nil || actuator.ovirtApi.Test() != nil {
		// session expired or some other error, re-login.
		actuator.ovirtApi, err = createApiConnection(actuator.client, namespace, secretName)
	}

	return actuator.ovirtApi, err
}

func (actuator *OvirtActuator) reconcileAnnotations(machine *machinev1.Machine, instance *clients.Instance) {
	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[InstanceStatusAnnotationKey] = string(instance.MustStatus())
}

func conditionSuccess() ovirtconfigv1.OvirtMachineProviderCondition {
	return ovirtconfigv1.OvirtMachineProviderCondition{
		Type:    ovirtconfigv1.MachineCreated,
		Status:  corev1.ConditionTrue,
		Reason:  "MachineCreateSucceeded",
		Message: "Machine successfully created",
	}
}

func conditionFailed() ovirtconfigv1.OvirtMachineProviderCondition {
	return ovirtconfigv1.OvirtMachineProviderCondition{
		Type:    ovirtconfigv1.MachineCreated,
		Status:  corev1.ConditionFalse,
		Reason:  "MachineCreateFailed",
		Message: "Machine creation failed",
	}
}
