package proxy

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	kstorage "k8s.io/apiserver/pkg/storage"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	kcoreclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/internalversion"
	"k8s.io/kubernetes/pkg/printers"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"
	nsregistry "k8s.io/kubernetes/pkg/registry/core/namespace"

	"github.com/openshift/api/project"
	"github.com/openshift/origin/pkg/api/apihelpers"
	authorizationapi "github.com/openshift/origin/pkg/authorization/apis/authorization"
	"github.com/openshift/origin/pkg/authorization/authorizer/scope"
	printersinternal "github.com/openshift/origin/pkg/printers/internalversion"
	projectapi "github.com/openshift/origin/pkg/project/apis/project"
	projectregistry "github.com/openshift/origin/pkg/project/apiserver/registry/project"
	projectauth "github.com/openshift/origin/pkg/project/auth"
	projectcache "github.com/openshift/origin/pkg/project/cache"
	projectutil "github.com/openshift/origin/pkg/project/util"
)

type REST struct {
	// client can modify Kubernetes namespaces
	client kcoreclient.NamespaceInterface
	// lister can enumerate project lists that enforce policy
	lister projectauth.Lister
	// Allows extended behavior during creation, required
	createStrategy rest.RESTCreateStrategy
	// Allows extended behavior during updates, required
	updateStrategy rest.RESTUpdateStrategy

	authCache    *projectauth.AuthorizationCache
	projectCache *projectcache.ProjectCache

	rest.TableConvertor
}

var _ rest.Lister = &REST{}
var _ rest.CreaterUpdater = &REST{}
var _ rest.GracefulDeleter = &REST{}
var _ rest.Watcher = &REST{}
var _ rest.Scoper = &REST{}

// NewREST returns a RESTStorage object that will work against Project resources
func NewREST(client kcoreclient.NamespaceInterface, lister projectauth.Lister, authCache *projectauth.AuthorizationCache, projectCache *projectcache.ProjectCache) *REST {
	return &REST{
		client:         client,
		lister:         lister,
		createStrategy: projectregistry.Strategy,
		updateStrategy: projectregistry.Strategy,

		authCache:    authCache,
		projectCache: projectCache,

		TableConvertor: printerstorage.TableConvertor{TablePrinter: printers.NewTablePrinter().With(printersinternal.AddHandlers)},
	}
}

// New returns a new Project
func (s *REST) New() runtime.Object {
	return &projectapi.Project{}
}

// NewList returns a new ProjectList
func (*REST) NewList() runtime.Object {
	return &projectapi.ProjectList{}
}

func (s *REST) NamespaceScoped() bool {
	return false
}

// List retrieves a list of Projects that match label.

func (s *REST) List(ctx context.Context, options *metainternal.ListOptions) (runtime.Object, error) {
	user, ok := apirequest.UserFrom(ctx)
	if !ok {
		return nil, kerrors.NewForbidden(project.Resource("project"), "", fmt.Errorf("unable to list projects without a user on the context"))
	}
	namespaceList, err := s.lister.List(user)
	if err != nil {
		return nil, err
	}
	m := nsregistry.MatchNamespace(apihelpers.InternalListOptionsToSelectors(options))
	list, err := filterList(namespaceList, m, nil)
	if err != nil {
		return nil, err
	}
	return projectutil.ConvertNamespaceList(list.(*kapi.NamespaceList)), nil
}

func (s *REST) Watch(ctx context.Context, options *metainternal.ListOptions) (watch.Interface, error) {
	if ctx == nil {
		return nil, fmt.Errorf("Context is nil")
	}
	userInfo, exists := apirequest.UserFrom(ctx)
	if !exists {
		return nil, fmt.Errorf("no user")
	}

	includeAllExistingProjects := (options != nil) && options.ResourceVersion == "0"

	allowedNamespaces, err := scope.ScopesToVisibleNamespaces(userInfo.GetExtra()[authorizationapi.ScopesKey], s.authCache.GetClusterRoleLister())
	if err != nil {
		return nil, err
	}

	watcher := projectauth.NewUserProjectWatcher(userInfo, allowedNamespaces, s.projectCache, s.authCache, includeAllExistingProjects)
	s.authCache.AddWatcher(watcher)

	go watcher.Watch()
	return watcher, nil
}

var _ = rest.Getter(&REST{})

// Get retrieves a Project by name
func (s *REST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	opts := metav1.GetOptions{}
	if options != nil {
		opts = *options
	}
	namespace, err := s.client.Get(name, opts)
	if err != nil {
		return nil, err
	}
	return projectutil.ConvertNamespace(namespace), nil
}

var _ = rest.Creater(&REST{})

// Create registers the given Project.
func (s *REST) Create(ctx context.Context, obj runtime.Object, creationValidation rest.ValidateObjectFunc, _ bool) (runtime.Object, error) {
	projectObj, ok := obj.(*projectapi.Project)
	if !ok {
		return nil, fmt.Errorf("not a project: %#v", obj)
	}
	rest.FillObjectMetaSystemFields(&projectObj.ObjectMeta)
	s.createStrategy.PrepareForCreate(ctx, obj)
	if errs := s.createStrategy.Validate(ctx, obj); len(errs) > 0 {
		return nil, kerrors.NewInvalid(project.Kind("Project"), projectObj.Name, errs)
	}
	if err := creationValidation(projectObj.DeepCopyObject()); err != nil {
		return nil, err
	}

	namespace, err := s.client.Create(projectutil.ConvertProject(projectObj))
	if err != nil {
		return nil, err
	}
	return projectutil.ConvertNamespace(namespace), nil
}

var _ = rest.Updater(&REST{})

func (s *REST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, creationValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc) (runtime.Object, bool, error) {
	oldObj, err := s.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}

	obj, err := objInfo.UpdatedObject(ctx, oldObj)
	if err != nil {
		return nil, false, err
	}

	projectObj, ok := obj.(*projectapi.Project)
	if !ok {
		return nil, false, fmt.Errorf("not a project: %#v", obj)
	}

	s.updateStrategy.PrepareForUpdate(ctx, obj, oldObj)
	if errs := s.updateStrategy.ValidateUpdate(ctx, obj, oldObj); len(errs) > 0 {
		return nil, false, kerrors.NewInvalid(project.Kind("Project"), projectObj.Name, errs)
	}
	if err := updateValidation(obj.DeepCopyObject(), oldObj.DeepCopyObject()); err != nil {
		return nil, false, err
	}

	namespace, err := s.client.Update(projectutil.ConvertProject(projectObj))
	if err != nil {
		return nil, false, err
	}

	return projectutil.ConvertNamespace(namespace), false, nil
}

var _ = rest.GracefulDeleter(&REST{})

// Delete deletes a Project specified by its name
func (s *REST) Delete(ctx context.Context, name string, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	return &metav1.Status{Status: metav1.StatusSuccess}, false, s.client.Delete(name, nil)
}

// decoratorFunc can mutate the provided object prior to being returned.
type decoratorFunc func(obj runtime.Object) error

// filterList filters any list object that conforms to the api conventions,
// provided that 'm' works with the concrete type of list. d is an optional
// decorator for the returned functions. Only matching items are decorated.
func filterList(list runtime.Object, m kstorage.SelectionPredicate, d decoratorFunc) (filtered runtime.Object, err error) {
	// TODO: push a matcher down into tools.etcdHelper to avoid all this
	// nonsense. This is a lot of unnecessary copies.
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	var filteredItems []runtime.Object
	for _, obj := range items {
		match, err := m.Matches(obj)
		if err != nil {
			return nil, err
		}
		if match {
			if d != nil {
				if err := d(obj); err != nil {
					return nil, err
				}
			}
			filteredItems = append(filteredItems, obj)
		}
	}
	err = meta.SetList(list, filteredItems)
	if err != nil {
		return nil, err
	}
	return list, nil
}
