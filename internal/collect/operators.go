package collect

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/imantaba/kubeagent/internal/operators"
)

// OperatorResources gates each adapter on API discovery, then lists the ones the
// cluster actually serves. Read-only: List calls only — never get, watch, or write.
//
// Discovery is the installation signal. An operator counts as installed when the
// API server serves its group, not because a Deployment is named after it, so an
// adapter whose group is absent is skipped with zero API calls, no error, and no
// report entry. A cluster running none of the six costs one discovery round trip.
//
// Discovery itself needs no RBAC grant: the default system:discovery ClusterRole
// is bound to system:authenticated on every conformant cluster. Listing the
// custom resources does — see deploy/rbac-operators.yaml. Without that grant the
// kind is marked Forbidden and the scan continues, which still answers "which
// operators are installed".
func OperatorResources(ctx context.Context, disco discovery.DiscoveryInterface,
	dyn dynamic.Interface, adapters []operators.Adapter, namespace string) []operators.Fetched {

	groups, err := disco.ServerGroups()
	if err != nil {
		// Discovery is open to every authenticated user, so a failure here means
		// the API server is unreachable — already the base scan's headline.
		return nil
	}
	served := map[string][]string{} // group → versions, the preferred one first
	for _, g := range groups.Groups {
		var vs []string
		if g.PreferredVersion.Version != "" {
			vs = append(vs, g.PreferredVersion.Version)
		}
		for _, v := range g.Versions {
			if v.Version != "" && !containsString(vs, v.Version) {
				vs = append(vs, v.Version)
			}
		}
		if len(vs) > 0 {
			served[g.Name] = vs
		}
	}

	// resourceScope caches one ServerResourcesForGroupVersion call per
	// group/version, so cert-manager's three adapters cost one round trip.
	resourceScope := map[string]map[string]bool{} // "group/version" → plural → namespaced
	var out []operators.Fetched

	for _, a := range adapters {
		versions, ok := served[a.Group]
		if !ok {
			continue // the group is not served: this operator is not installed
		}
		// Version tolerance: prefer the version the adapter names, else take the
		// group's preferred one. A field path missing from an unfamiliar version
		// yields unknown, which is the designed degradation.
		version := versions[0]
		if containsString(versions, a.Version) {
			version = a.Version
		}
		gv := a.Group + "/" + version
		if _, cached := resourceScope[gv]; !cached {
			scope := map[string]bool{}
			if list, err := disco.ServerResourcesForGroupVersion(gv); err == nil {
				for _, r := range list.APIResources {
					scope[r.Name] = r.Namespaced
				}
			}
			resourceScope[gv] = scope
		}
		namespaced, serves := resourceScope[gv][a.Resource]
		if !serves {
			continue // the group is served but this CRD is not installed
		}

		gvr := schema.GroupVersionResource{Group: a.Group, Version: version, Resource: a.Resource}
		var ri dynamic.ResourceInterface = dyn.Resource(gvr)
		if namespaced && namespace != "" {
			ri = dyn.Resource(gvr).Namespace(namespace)
		}

		f := operators.Fetched{Adapter: a, APIVersion: gv}
		list, err := ri.List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsForbidden(err):
			// One denial marks its own kind and nothing else: a missing grant on
			// one CRD must never fail the scan or another operator.
			f.Forbidden = true
		case err != nil:
			f.Err = err
		default:
			f.Items = list.Items
		}
		out = append(out, f)
	}
	return out
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
