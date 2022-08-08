package namecache

import (
	"context"
	"fmt"
	"sync"

	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/config"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	MappingAnnotation = "vcluster.loft.sh/name-mappings"
)

type NameCache interface {
	ResolveName(hostName string) string
}

func NewNameCache(ctx context.Context, manager ctrl.Manager, mapping *config.Mapping) (NameCache, error) {
	nc := &nameCache{
		manager:            manager,
		hostToVirtualNames: map[string][]*virtualObject{},
		virtualObjects:     map[string]*virtualObject{},
	}

	if mapping.FromVirtualCluster != nil {
		found := false

		// check if there is at least 1 reverse patch that would use the cache
		for _, p := range mapping.FromVirtualCluster.ReversePatches {
			if p.Type == config.PatchTypeRewriteName || p.Type == config.PatchTypeRewriteNamespace {
				found = true
				break
			}
		}
		// TODO: add checks of syncBack - Phase 2
		if !found {
			return nc, nil
		}

		// add informer to cache
		gvk := schema.FromAPIVersionAndKind(mapping.FromVirtualCluster.ApiVersion, mapping.FromVirtualCluster.Kind)
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(mapping.FromVirtualCluster.ApiVersion)
		obj.SetKind(mapping.FromVirtualCluster.Kind)
		informer, err := nc.manager.GetCache().GetInformer(ctx, obj)
		if err != nil {
			return nil, fmt.Errorf("get informer for %v: %v", gvk, err)
		}

		informer.AddEventHandler(&fromVirtualClusterCacheHandler{
			gvk:       gvk,
			mapping:   mapping.FromVirtualCluster,
			nameCache: nc,
		})
	} else if mapping.FromHostCluster != nil {
		// check if there is at least 1 mapping
		if mapping.FromHostCluster.NameMapping.RewriteName != config.RewriteNameTypeFromHostToVirtualNamespace {
			// check if there is a patch that rewrites a name
			found := false
			for _, p := range mapping.FromVirtualCluster.Patches {
				if p.Type == config.RewriteNameTypeFromHostToVirtualNamespace {
					found = true
					break
				}
			}
			if !found {
				return nc, nil
			}
		}

		// add informer to cache
		gvk := schema.FromAPIVersionAndKind(mapping.FromHostCluster.ApiVersion, mapping.FromHostCluster.Kind)
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(mapping.FromHostCluster.ApiVersion)
		obj.SetKind(mapping.FromHostCluster.Kind)
		informer, err := nc.manager.GetCache().GetInformer(ctx, obj)
		if err != nil {
			return nil, errors.Wrapf(err, "get informer for %v", gvk)
		}

		informer.AddEventHandler(&fromHostClusterCacheHandler{
			gvk:       gvk,
			mapping:   mapping.FromVirtualCluster,
			nameCache: nc,
		})
	}

	return nc, nil
}

type nameCache struct {
	manager ctrl.Manager

	m                  sync.Mutex
	hostToVirtualNames map[string][]*virtualObject
	virtualObjects     map[string]*virtualObject
}

type virtualObject struct {
	GVK         schema.GroupVersionKind
	VirtualName string

	Mappings []mapping
}

type mapping struct {
	// VirtualName holds the name in format Namespace/Name
	VirtualName string
	// HostName holds the generated name in format Name
	HostName string
}

func (v virtualObject) String() string {
	return v.VirtualName + "/" + v.GVK.String()
}

func (n *nameCache) ResolveName(hostName string) string {
	n.m.Lock()
	defer n.m.Unlock()

	slice := n.hostToVirtualNames[hostName]
	if len(slice) > 0 {
		for _, m := range slice[0].Mappings {
			if m.HostName == hostName {
				return m.VirtualName
			}
		}
	}

	return ""
}

func (n *nameCache) RemoveMapping(object *virtualObject) {
	n.m.Lock()
	defer n.m.Unlock()

	n.removeMapping(object)
}

func (n *nameCache) removeMapping(object *virtualObject) {
	name := object.String()
	oldVirtualObject, ok := n.virtualObjects[name]
	if ok {
		delete(n.virtualObjects, name)

		for _, mapping := range oldVirtualObject.Mappings {
			slice, ok := n.hostToVirtualNames[mapping.HostName]
			if ok {
				if len(slice) == 0 {
					delete(n.hostToVirtualNames, mapping.HostName)
					continue
				} else if len(slice) == 1 && slice[0].String() == name {
					delete(n.hostToVirtualNames, mapping.HostName)
					continue
				}

				otherObjects := []*virtualObject{}
				for _, oldObject := range slice {
					if oldObject.String() == name {
						continue
					}
					otherObjects = append(otherObjects, oldObject)
				}
				if len(slice) == 0 {
					delete(n.hostToVirtualNames, mapping.HostName)
					continue
				}
				n.hostToVirtualNames[mapping.HostName] = otherObjects
			}
		}
	}
}

func (n *nameCache) exchangeMapping(object *virtualObject) {
	n.m.Lock()
	defer n.m.Unlock()

	name := object.String()
	oldVirtualObject, ok := n.virtualObjects[name]
	if ok && equality.Semantic.DeepEqual(object, oldVirtualObject) {
		return
	} else if ok {
		// remove
		n.removeMapping(object)
	}

	// add
	if len(object.Mappings) > 0 {
		n.virtualObjects[object.String()] = object
		for _, m := range object.Mappings {
			slice, ok := n.hostToVirtualNames[m.HostName]
			if ok {
				slice = append(slice, object)
				n.hostToVirtualNames[m.HostName] = slice
			} else {
				n.hostToVirtualNames[m.HostName] = []*virtualObject{object}
			}
		}
	}
}
