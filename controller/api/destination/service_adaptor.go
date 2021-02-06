package destination

import (
	"github.com/linkerd/linkerd2/controller/api/destination/watcher"
	sp "github.com/linkerd/linkerd2/controller/gen/apis/serviceprofile/v1alpha2"
)

type serviceAdaptor struct {
	listener     watcher.ProfileUpdateListener
	profile      *sp.ServiceProfile
	servicePorts map[uint32]struct{}
}

func newServiceAdaptor(listener watcher.ProfileUpdateListener) *serviceAdaptor {
	return &serviceAdaptor{
		listener: listener,
	}
}

func (sa *serviceAdaptor) Update(profile *sp.ServiceProfile) {
	sa.profile = profile
	sa.publish()
}

func (sa *serviceAdaptor) UpdateService(ports map[uint32]struct{}) {
	sa.servicePorts = ports
	sa.publish()
}

func (sa *serviceAdaptor) publish() {
	merged := sp.ServiceProfile{}
	if sa.profile != nil {
		merged = *sa.profile
	}
	if len(sa.servicePorts) != 0 {
		merged.Spec.OpaquePorts = sa.servicePorts
	}
	sa.listener.Update(&merged)
}
