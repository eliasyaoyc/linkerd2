package watcher

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"github.com/linkerd/linkerd2/controller/k8s"
	labels "github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/util"
	logging "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ServiceWatcher TODO
type ServiceWatcher struct {
	opSubscriptions map[string]*nsSubscriptions
	k8sAPI          *k8s.API
	log             *logging.Entry
	sync.RWMutex
}

type nsSubscriptions struct {
	opaquePorts map[uint32]struct{}
	services    map[ServiceID]*svcSubscriptions
}

type svcSubscriptions struct {
	opaquePorts map[uint32]struct{}
	listeners   []ServiceUpdateListener
}

// ServiceUpdateListener is the interface that subscribers must implement.
type ServiceUpdateListener interface {
	UpdateService(ports map[uint32]struct{})
}

// NewServiceWatcher TODO
func NewServiceWatcher(k8sAPI *k8s.API, log *logging.Entry) *ServiceWatcher {
	sw := &ServiceWatcher{
		opSubscriptions: make(map[string]*nsSubscriptions),
		k8sAPI:          k8sAPI,
		log:             log.WithField("component", "service-watcher"),
	}
	k8sAPI.Svc().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    sw.addService,
		DeleteFunc: sw.deleteService,
		UpdateFunc: func(_, obj interface{}) { sw.addService(obj) },
	})
	k8sAPI.NS().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    sw.addNamespace,
		DeleteFunc: sw.deleteNamespace,
		UpdateFunc: func(_, obj interface{}) { sw.addNamespace(obj) },
	})
	return sw
}

// Subscribe TODO
func (sw *ServiceWatcher) Subscribe(id ServiceID, listener ServiceUpdateListener) error {
	sw.Lock()
	defer sw.Unlock()
	svc, _ := sw.k8sAPI.Svc().Lister().Services(id.Namespace).Get(id.Name)
	if svc != nil && svc.Spec.Type == corev1.ServiceTypeExternalName {
		return invalidService(id.String())
	}
	sw.log.Infof("Establishing watch on service %s", id)
	ns, ok := sw.opSubscriptions[id.Namespace]
	// If there is no watched namespace for the service, create a subscription
	// for the namespace qualified service and no opaque ports.
	if !ok {
		sw.opSubscriptions[id.Namespace] = &nsSubscriptions{
			opaquePorts: make(map[uint32]struct{}),
			services: map[ServiceID]*svcSubscriptions{id: {
				opaquePorts: make(map[uint32]struct{}),
				listeners:   []ServiceUpdateListener{listener},
			}},
		}
		return nil
	}
	ss, ok := ns.services[id]
	// If there is no watched service, create a subscription for the service
	// and no opaque ports.
	if !ok {
		ns.services[id] = &svcSubscriptions{
			opaquePorts: make(map[uint32]struct{}),
			listeners:   []ServiceUpdateListener{listener},
		}
		if len(ns.opaquePorts) != 0 {
			listener.UpdateService(ns.opaquePorts)
		}
		return nil
	}
	// There are subscriptions for this service, so add the listener to the
	// service listeners. If there are opaque ports for the service or the
	// namespace, update the listener with that value.
	ss.listeners = append(ss.listeners, listener)
	op := ss.opaquePorts
	if len(op) == 0 {
		op = ns.opaquePorts
	}
	if len(op) != 0 {
		listener.UpdateService(op)
	}
	return nil
}

// Unsubscribe TODO
func (sw *ServiceWatcher) Unsubscribe(id ServiceID, listener ServiceUpdateListener) {
	sw.Lock()
	defer sw.Unlock()
	sw.log.Infof("Stopping watch on service [%s]", id)
	ns, ok := sw.opSubscriptions[id.Namespace]
	if !ok {
		sw.log.Errorf("Cannot unsubscribe from service in unknown namespace %s", id.Namespace)
		return
	}
	ss, ok := ns.services[id]
	if !ok {
		sw.log.Errorf("Cannot unsubscribe from unknown service %s", id)
		return
	}
	for i, l := range ss.listeners {
		if l == listener {
			n := len(ss.listeners)
			ss.listeners[i] = ss.listeners[n-1]
			ss.listeners[n-1] = nil
			ss.listeners = ss.listeners[:n-1]
		}
	}
	fmt.Printf("service=%v, subscribes=%v", id, len(ss.listeners))
}

func (sw *ServiceWatcher) addService(obj interface{}) {
	sw.Lock()
	defer sw.Unlock()
	svc := obj.(*corev1.Service)
	if svc.Namespace == kubeSystem {
		return
	}
	id := ServiceID{
		Namespace: svc.Namespace,
		Name:      svc.Name,
	}
	opaquePorts, err := getServiceOpaquePortsAnnotations(svc)
	if err != nil {
		sw.log.Errorf("failed to get %s service's opaque ports annotation: %s", id, err)
		return
	}
	// If the service has no opaque ports, we check the namespace. If the
	// namespace does have the service, that means there are listeners waiting
	// for updates; we must update them with the namespace's opaque ports.
	if len(opaquePorts) == 0 {
		ns, ok := sw.opSubscriptions[id.Namespace]
		// If there are no namespace subscriptions or the namespace has no
		// opaque ports, there are no listeners to update.
		if !ok || len(ns.opaquePorts) == 0 {
			return
		}
		ss, ok := ns.services[id]
		// There are no listeners for this service.
		if !ok {
			return
		}
		for _, listener := range ss.listeners {
			listener.UpdateService(ns.opaquePorts)
		}
		return
	}
	ns, ok := sw.opSubscriptions[id.Namespace]
	// If there are no namespace subscriptions for the service's namespace,
	// create one and add the service subscription with its opaque ports.
	if !ok {
		sw.opSubscriptions[id.Namespace] = &nsSubscriptions{
			opaquePorts: make(map[uint32]struct{}),
			services: map[ServiceID]*svcSubscriptions{id: {
				opaquePorts: opaquePorts,
				listeners:   []ServiceUpdateListener{},
			}},
		}
		return
	}
	ss, ok := ns.services[id]
	// If there is service subscription, create one with the opaque ports.
	if !ok {
		ns.services[id] = &svcSubscriptions{
			opaquePorts: opaquePorts,
			listeners:   []ServiceUpdateListener{},
		}
		return
	}
	// Do not send updates if there was no change in the opaque ports; if
	// there was, send an update to each of the listeners.
	if !reflect.DeepEqual(ss.opaquePorts, opaquePorts) {
		ss.opaquePorts = opaquePorts
		for _, listener := range ss.listeners {
			listener.UpdateService(ss.opaquePorts)
		}
	}

}

func (sw *ServiceWatcher) deleteService(obj interface{}) {
	sw.Lock()
	defer sw.Unlock()
	service, ok := obj.(*corev1.Service)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			sw.log.Errorf("couldn't get object from DeletedFinalStateUnknown %#v", obj)
			return
		}
		service, ok = tombstone.Obj.(*corev1.Service)
		if !ok {
			sw.log.Errorf("DeletedFinalStateUnknown contained object that is not a Service %#v", obj)
			return
		}
	}
	if service.Namespace == kubeSystem {
		return
	}
	id := ServiceID{
		Namespace: service.Namespace,
		Name:      service.Name,
	}
	ns, ok := sw.opSubscriptions[id.Namespace]
	if !ok {
		return
	}
	ss, ok := ns.services[id]
	if !ok {
		return
	}
	ss.opaquePorts = make(map[uint32]struct{})
	fmt.Printf("deleting %v service; ns.op=%v, listeners=%v\n", id, ns.opaquePorts, len(ss.listeners))
	// Deleting a service does not mean there are no opaque ports; if the
	// namespace has a list, that must be sent instead.
	if len(ns.opaquePorts) != 0 {
		for _, listener := range ss.listeners {
			listener.UpdateService(ns.opaquePorts)
		}
		return
	}
	for _, listener := range ss.listeners {
		listener.UpdateService(make(map[uint32]struct{}))
	}
}

func (sw *ServiceWatcher) addNamespace(obj interface{}) {
	sw.Lock()
	defer sw.Unlock()
	namespace := obj.(*corev1.Namespace)
	opaquePorts, err := getNamespaceOpaquePortsAnnotations(namespace)
	if err != nil {
		sw.log.Errorf("failed to get %s namespaces's opaque ports annotation: %s", namespace.Name, err)
		return
	}
	// If there are no opaque ports on the namespaces, there is nothing to do.
	if len(opaquePorts) == 0 {
		return
	}
	ns, ok := sw.opSubscriptions[namespace.Name]
	// If there are no namespace subscriptions, there are no listeners to
	// update; we do set the opaque ports though.
	if !ok {
		sw.opSubscriptions[namespace.Name] = &nsSubscriptions{
			opaquePorts: opaquePorts,
			services:    make(map[ServiceID]*svcSubscriptions),
		}
	}
	if ns == nil {
		ns = &nsSubscriptions{}
	}
	ns.opaquePorts = opaquePorts
	fmt.Printf("adding %v namespace; op=%v\n", namespace.Name, ns.opaquePorts)
	// For each service subscribed to in the namespace, send an update with
	// the namespace's opaque ports only if the service does not have its own.
	for _, svc := range ns.services {
		if len(svc.opaquePorts) == 0 {
			for _, listener := range svc.listeners {
				listener.UpdateService(ns.opaquePorts)
			}
		}
	}
}

func (sw *ServiceWatcher) deleteNamespace(obj interface{}) {
	sw.Lock()
	defer sw.Unlock()
	namespace := obj.(*corev1.Namespace)
	ns, ok := sw.opSubscriptions[namespace.Name]
	// If there are no namespace subscriptions, there are no listeners to
	// update.
	if !ok {
		return
	}
	if ns == nil {
		ns = &nsSubscriptions{}
	}
	ns.opaquePorts = make(map[uint32]struct{})
	// For each service subscribed to in the namespace, send an update with
	// the namespace's opaque ports only if the service does not have its own.
	//
	// Note: At this point if the namespace is being deleted, then the
	// services within that namespace have likely been deleted. In this case,
	// each service will have no opaque ports, but it's still important that
	// the updates are sent. Since the stream remains open, clients must be
	// updated that the service is not an opaque protocol.
	for _, svc := range ns.services {
		if len(svc.opaquePorts) == 0 {
			for _, listener := range svc.listeners {
				listener.UpdateService(ns.opaquePorts)
			}
		}
	}
}

func getServiceOpaquePortsAnnotations(service *corev1.Service) (map[uint32]struct{}, error) {
	opaquePorts := make(map[uint32]struct{})
	annotation := service.Annotations[labels.ProxyOpaquePortsAnnotation]
	if annotation != "" {
		for _, portStr := range util.ParseOpaquePorts(annotation) {
			port, err := strconv.ParseUint(portStr, 10, 32)
			if err != nil {
				return nil, err
			}
			opaquePorts[uint32(port)] = struct{}{}
		}
	}
	return opaquePorts, nil
}

func getNamespaceOpaquePortsAnnotations(ns *corev1.Namespace) (map[uint32]struct{}, error) {
	opaquePorts := make(map[uint32]struct{})
	annotation := ns.Annotations[labels.ProxyOpaquePortsAnnotation]
	if annotation != "" {
		for _, portStr := range util.ParseOpaquePorts(annotation) {
			port, err := strconv.ParseUint(portStr, 10, 32)
			if err != nil {
				return nil, err
			}
			opaquePorts[uint32(port)] = struct{}{}
		}
	}
	return opaquePorts, nil
}
