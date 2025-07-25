/*
Copyright 2020 The Crossplane Authors.

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

package manager

import (
	"context"
	"io"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/conditions"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	v1 "github.com/crossplane/crossplane/apis/pkg/v1"
	"github.com/crossplane/crossplane/internal/xpkg/fake"
)

var _ Revisioner = &MockRevisioner{}

type MockRevisioner struct {
	MockRevision func() (string, error)
}

func NewMockRevisionFn(hash string, err error) func() (string, error) {
	return func() (string, error) {
		return hash, err
	}
}

func (m *MockRevisioner) Revision(context.Context, v1.Package, ...string) (string, error) {
	return m.MockRevision()
}

func TestReconcile(t *testing.T) {
	errBoom := errors.New("boom")
	testLog := logging.NewLogrLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(io.Discard)).WithName("testlog"))
	pullAlways := corev1.PullAlways
	trueVal := true
	revHistory := int64(1)

	type args struct {
		req reconcile.Request
		rec *Reconciler
	}
	type want struct {
		r   reconcile.Result
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"PackageNotFound": {
			reason: "We should not return and error and not requeue if package not found.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage: func() v1.Package { return &v1.Configuration{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{MockGet: test.NewMockGetFn(kerrors.NewNotFound(schema.GroupResource{}, ""))},
					},
					log:        testLog,
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ErrGetPackage": {
			reason: "We should return an error if getting package fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage: func() v1.Package { return &v1.Configuration{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{MockGet: test.NewMockGetFn(errBoom)},
					},
					log:        testLog,
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetPackage),
			},
		},
		"ErrListRevisions": {
			reason: "We should return an error if listing revisions for a package fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet:  test.NewMockGetFn(nil),
							MockList: test.NewMockListFn(errBoom),
						},
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errListRevisions),
			},
		},
		"ErrRewritePath": {
			reason: "We should return an error if rewriting the image path based on configs fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet:  test.NewMockGetFn(nil),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetConditions(v1.Unpacking().WithMessage(errors.Wrap(errBoom, errRewriteImage).Error()))
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("", errBoom),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", errBoom),
					},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errRewriteImage),
			},
		},
		"ErrGetPullConfig": {
			reason: "We should return an error if getting the pull secret from image configs.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet:  test.NewMockGetFn(nil),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetConditions(v1.Unpacking().WithMessage(errors.Wrap(errBoom, errGetPullConfig).Error()))
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					log:    testLog,
					record: event.NewNopRecorder(),
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("", errBoom),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", errBoom),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetPullConfig),
			},
		},
		"ErrFetchRevision": {
			reason: "We should return an error if fetching the revision for a package fails.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet:  test.NewMockGetFn(nil),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetConditions(v1.Unpacking().WithMessage(errors.Wrap(errBoom, errUnpack).Error()))
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					log:    testLog,
					record: event.NewNopRecorder(),
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("", errBoom),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errUnpack),
			},
		},
		"SuccessfulRerwiteImage": {
			reason: "We should record the rewritten image path if an image config is used.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.AutomaticActivation)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetActivationPolicy(&v1.AutomaticActivation)
								want.SetConditions(v1.Unhealthy().WithMessage("Package revision health is \"Unknown\""))
								want.SetConditions(v1.Active())
								want.SetResolvedSource("new/image/path")
								want.SetAppliedImageConfigRefs(v1.ImageConfigRef{
									Name:   "imageConfigName",
									Reason: v1.ImageConfigReasonRewrite,
								})
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("imageConfigName", "new/image/path", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SuccessfulNoExistingRevisionsAutoActivate": {
			reason: "We should be active and not requeue on successful creation of the first revision with auto activation.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.AutomaticActivation)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetActivationPolicy(&v1.AutomaticActivation)
								want.SetConditions(v1.Unhealthy().WithMessage("Package revision health is \"Unknown\""))
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SuccessfulNoExistingRevisionsAutoActivatePullAlways": {
			reason: "We should be active and requeue after wait on successful creation of the first revision with auto activation and package pull policy Always.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.AutomaticActivation)
								p.SetPackagePullPolicy(&pullAlways)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetActivationPolicy(&v1.AutomaticActivation)
								want.SetPackagePullPolicy(&pullAlways)
								want.SetConditions(v1.Unhealthy().WithMessage("Package revision health is \"Unknown\""))
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{RequeueAfter: pullWait},
			},
		},
		"SuccessfulNoExistingRevisionsManualActivate": {
			reason: "We should be inactive and not requeue on successful creation of the first revision with manual activation policy.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.ManualActivation)
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetActivationPolicy(&v1.ManualActivation)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Unhealthy().WithMessage("Package revision health is \"Unknown\""))
								want.SetConditions(v1.Inactive().WithMessage("Package is inactive"))
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SuccessfulActiveRevisionExists": {
			reason: "We should match revision health and not requeue when active revision already exists.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetConditions(v1.RevisionHealthy())
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Healthy())
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SuccessfulRevisionExistsNeedsActive": {
			reason: "We should match revision health, set to active, and not requeue when inactive revision already exists and activation policy is automatic.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1.RevisionHealthy())
								cr.SetDesiredState(v1.PackageRevisionInactive)
								cr.SetRevision(1)
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Healthy())
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, o client.Object, _ ...resource.ApplyOption) error {
							want := &v1.ConfigurationRevision{}
							want.SetLabels(map[string]string{"pkg.crossplane.io/package": "test"})
							want.SetName("test-1234567")
							want.SetOwnerReferences([]metav1.OwnerReference{{
								APIVersion:         v1.SchemeGroupVersion.String(),
								Kind:               v1.ConfigurationKind,
								Name:               "test",
								Controller:         &trueVal,
								BlockOwnerDeletion: &trueVal,
							}})
							want.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
							want.SetDesiredState(v1.PackageRevisionActive)
							want.SetConditions(v1.RevisionHealthy())
							want.SetRevision(1)
							if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
								t.Errorf("-want, +got:\n%s", diff)
							}
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ErrUpdatePackageRevision": {
			reason: "Failing to update a package revision should cause us to return an error.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1.RevisionHealthy())
								cr.SetDesiredState(v1.PackageRevisionInactive)
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Healthy())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return errBoom
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errApplyPackageRevision),
			},
		},
		"SuccessfulTransitionUnhealthy": {
			reason: "If the current revision is unhealthy the package should be also.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1.RevisionUnhealthy().WithMessage("some message"))
								cr.SetDesiredState(v1.PackageRevisionActive)
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{cr},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Unhealthy().WithMessage("Package revision health is \"False\" with message: some message"))
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"SuccessfulRevisionExistsNeedGC": {
			reason: "We should successfully garbage collect when an old revision falls outside range.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetRevision(3)
								cr.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1.RevisionHealthy())
								cr.SetDesiredState(v1.PackageRevisionInactive)
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{
										cr,
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "made-the-cut",
											},
											Spec: v1.PackageRevisionSpec{
												Revision: 2,
											},
										},
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "missed-the-cut",
											},
											Spec: v1.PackageRevisionSpec{
												Revision: 1,
											},
										},
									},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetConditions(v1.Healthy())
								want.SetConditions(v1.Active())
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
							MockDelete: test.NewMockDeleteFn(nil),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, o client.Object, _ ...resource.ApplyOption) error {
							want := &v1.ConfigurationRevision{}
							want.SetLabels(map[string]string{"pkg.crossplane.io/package": "test"})
							want.SetName("test-1234567")
							want.SetOwnerReferences([]metav1.OwnerReference{{
								APIVersion:         v1.SchemeGroupVersion.String(),
								Kind:               v1.ConfigurationKind,
								Name:               "test",
								Controller:         &trueVal,
								BlockOwnerDeletion: &trueVal,
							}})
							want.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
							want.SetDesiredState(v1.PackageRevisionActive)
							want.SetConditions(v1.RevisionHealthy())
							want.SetRevision(3)
							if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
								t.Errorf("-want, +got:\n%s", diff)
							}
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ErrGC": {
			reason: "Failure to garbage collect old package revision should cause return an error.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetRevisionHistoryLimit(&revHistory)
								return nil
							}),
							MockList: test.NewMockListFn(nil, func(o client.ObjectList) error {
								l := o.(*v1.ConfigurationRevisionList)
								cr := v1.ConfigurationRevision{
									ObjectMeta: metav1.ObjectMeta{
										Name: "test-1234567",
									},
								}
								cr.SetRevision(3)
								cr.SetGroupVersionKind(v1.ConfigurationRevisionGroupVersionKind)
								cr.SetConditions(v1.RevisionHealthy())
								cr.SetDesiredState(v1.PackageRevisionInactive)
								c := v1.ConfigurationRevisionList{
									Items: []v1.ConfigurationRevision{
										cr,
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "made-the-cut",
											},
											Spec: v1.PackageRevisionSpec{
												Revision:     2,
												DesiredState: v1.PackageRevisionInactive,
											},
										},
										{
											ObjectMeta: metav1.ObjectMeta{
												Name: "missed-the-cut",
											},
											Spec: v1.PackageRevisionSpec{
												Revision:     1,
												DesiredState: v1.PackageRevisionInactive,
											},
										},
									},
								}
								*l = c
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetCurrentRevision("test-1234567")
								want.SetRevisionHistoryLimit(&revHistory)
								if diff := cmp.Diff(want, o, test.EquateConditions()); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
							MockDelete: test.NewMockDeleteFn(errBoom),
						},
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGCPackageRevision),
			},
		},
		"PauseReconcile": {
			reason: "Pause reconciliation if the pause annotation is set",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.AutomaticActivation)
								p.SetAnnotations(map[string]string{
									meta.AnnotationKeyReconciliationPaused: "true",
								})
								return nil
							}),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetAnnotations(map[string]string{
									meta.AnnotationKeyReconciliationPaused: "true",
								})
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetActivationPolicy(&v1.AutomaticActivation)
								want.SetConditions(commonv1.ReconcilePaused().WithMessage(reconcilePausedMsg))
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
		"ResumeReconcile": {
			reason: "We should be active and not requeue on successful creation of the first revision with auto activation.",
			args: args{
				req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "test"}},
				rec: &Reconciler{
					newPackage:             func() v1.Package { return &v1.Configuration{} },
					newPackageRevision:     func() v1.PackageRevision { return &v1.ConfigurationRevision{} },
					newPackageRevisionList: func() v1.PackageRevisionList { return &v1.ConfigurationRevisionList{} },
					client: resource.ClientApplicator{
						Client: &test.MockClient{
							MockGet: test.NewMockGetFn(nil, func(o client.Object) error {
								p := o.(*v1.Configuration)
								p.SetName("test")
								p.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								p.SetActivationPolicy(&v1.AutomaticActivation)
								p.SetConditions(commonv1.ReconcilePaused())
								return nil
							}),
							MockList: test.NewMockListFn(kerrors.NewNotFound(schema.GroupResource{}, "")),
							MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil, func(o client.Object) error {
								want := &v1.Configuration{}
								want.SetName("test")
								want.SetGroupVersionKind(v1.ConfigurationGroupVersionKind)
								want.SetActivationPolicy(&v1.AutomaticActivation)
								want.Status.Conditions = []commonv1.Condition{}
								if diff := cmp.Diff(want, o); diff != "" {
									t.Errorf("-want, +got:\n%s", diff)
								}
								return nil
							}),
						},
						Applicator: resource.ApplyFn(func(_ context.Context, _ client.Object, _ ...resource.ApplyOption) error {
							return nil
						}),
					},
					pkg: &MockRevisioner{
						MockRevision: NewMockRevisionFn("test-1234567", nil),
					},
					config: &fake.MockConfigStore{
						MockPullSecretFor: fake.NewMockConfigStorePullSecretForFn("", "", nil),
						MockRewritePath:   fake.NewMockRewritePathFn("", "", nil),
					},
					log:        testLog,
					record:     event.NewNopRecorder(),
					conditions: conditions.ObservedGenerationPropagationManager{},
				},
			},
			want: want{
				r: reconcile.Result{Requeue: false},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.args.rec.Reconcile(context.Background(), reconcile.Request{})

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want error, +got error:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.r, got, test.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nr.Reconcile(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
