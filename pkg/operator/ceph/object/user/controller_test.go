/*
Copyright 2018 The Rook Authors. All rights reserved.

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

// Package objectuser to manage a rook object store.
package objectuser

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rgw/admin"
	"github.com/coreos/pkg/capnslog"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookclient "github.com/rook/rook/pkg/client/clientset/versioned/fake"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/operator/test"

	"github.com/rook/rook/pkg/clusterd"
	cephobject "github.com/rook/rook/pkg/operator/ceph/object"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	userCreateJSON = `{
	"user_id": "my-user",
	"display_name": "my-user",
	"email": "",
	"suspended": 0,
	"max_buckets": 1000,
	"subusers": [],
	"keys": [
		{
			"user": "my-user",
			"access_key": "EOE7FYCNOBZJ5VFV909G",
			"secret_key": "qmIqpWm8HxCzmynCrD6U6vKWi4hnDBndOnmxXNsV"
		}
	],
	"swift_keys": [],
	"caps": [],
	"op_mask": "read, write, delete",
	"default_placement": "",
	"default_storage_class": "",
	"placement_tags": [],
	"bucket_quota": {
		"enabled": false,
		"check_on_raw": false,
		"max_size": -1,
		"max_size_kb": 0,
		"max_objects": -1
	},
	"user_quota": {
		"enabled": false,
		"check_on_raw": false,
		"max_size": -1,
		"max_size_kb": 0,
		"max_objects": -1
	},
	"temp_url_keys": [],
	"type": "rgw",
	"mfa_ids": []
}`
)

var (
	name      = "my-user"
	namespace = "rook-ceph"
	store     = "my-store"
)

func TestCephObjectStoreUserController(t *testing.T) {
	ctx := context.TODO()
	// Set DEBUG logging
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)

	//
	// TEST 1 SETUP
	//
	// FAILURE because no CephCluster
	//
	// A Pool resource with metadata and spec.
	objectUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephObjectStoreUser",
		},
	}
	cephCluster := &cephv1.CephCluster{}

	// Objects to track in the fake client.
	object := []runtime.Object{
		objectUser,
		cephCluster,
	}

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(command, outfile string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_ERR"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			return "", nil
		},
	}
	clientset := test.New(t, 3)
	c := &clusterd.Context{
		Executor:      executor,
		RookClientset: rookclient.NewSimpleClientset(),
		Clientset:     clientset,
	}

	// Register operator types with the runtime scheme.
	s := scheme.Scheme
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStoreUser{}, &cephv1.CephCluster{}, &cephv1.CephClusterList{})

	// Create a fake client to mock API calls.
	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
	// Create a ReconcileObjectStoreUser object with the scheme and fake client.
	r := &ReconcileObjectStoreUser{client: cl, scheme: s, context: c}

	// Mock request to simulate Reconcile() being called on an event for a
	// watched resource .
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}
	logger.Info("STARTING PHASE 1")
	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.True(t, res.Requeue)
	logger.Info("PHASE 1 DONE")

	//
	// TEST 2:
	//
	// FAILURE we have a cluster but it's not ready
	//
	cephCluster = &cephv1.CephCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace,
			Namespace: namespace,
		},
		Status: cephv1.ClusterStatus{
			Phase: "",
			CephStatus: &cephv1.CephStatus{
				Health: "",
			},
		},
	}
	object = append(object, cephCluster)
	// Create a fake client to mock API calls.
	cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
	// Create a ReconcileObjectStoreUser object with the scheme and fake client.
	r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c}
	logger.Info("STARTING PHASE 2")
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.True(t, res.Requeue)
	logger.Info("PHASE 2 DONE")

	//
	// TEST 3:
	//
	// FAILURE! The CephCluster is ready but NO rgw object
	//

	// Mock clusterInfo
	secrets := map[string][]byte{
		"fsid":         []byte(name),
		"mon-secret":   []byte("monsecret"),
		"admin-secret": []byte("adminsecret"),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rook-ceph-mon",
			Namespace: namespace,
		},
		Data: secrets,
		Type: k8sutil.RookType,
	}
	_, err = c.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	// Add ready status to the CephCluster
	cephCluster.Status.Phase = k8sutil.ReadyStatus
	cephCluster.Status.CephStatus.Health = "HEALTH_OK"

	// Create a fake client to mock API calls.
	cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()

	executor = &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(command, outfile string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_OK"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			return "", nil
		},
		MockExecuteCommandWithTimeout: func(timeout time.Duration, command string, args ...string) (string, error) {
			if args[0] == "user" {
				return userCreateJSON, nil
			}
			return "", nil
		},
	}
	c.Executor = executor

	// Create a ReconcileObjectStoreUser object with the scheme and fake client.
	r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c}

	logger.Info("STARTING PHASE 3")
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.True(t, res.Requeue)
	logger.Info("PHASE 3 DONE")

	//
	// TEST 4:
	//
	// FAILURE! The CephCluster is ready
	// Rgw object exists but NO pod are running
	//
	cephObjectStore := &cephv1.CephObjectStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      store,
			Namespace: namespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephObjectStore",
		},
		Spec: cephv1.ObjectStoreSpec{
			Gateway: cephv1.GatewaySpec{
				Port: 80,
			},
		},
		Status: &cephv1.ObjectStoreStatus{
			Info: map[string]string{"endpoint": "http://rook-ceph-rgw-my-store.rook-ceph:80"},
		},
	}
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStore{})
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStoreList{})
	object = append(object, cephObjectStore)

	// Create a fake client to mock API calls.
	cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
	// Create a ReconcileObjectStoreUser object with the scheme and fake client.
	r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c}

	logger.Info("STARTING PHASE 4")
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: store, Namespace: namespace}, cephObjectStore)
	assert.NoError(t, err, cephObjectStore)
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.True(t, res.Requeue)
	logger.Info("PHASE 4 DONE")

	//
	// TEST 5:
	//
	// SUCCESS! The CephCluster is ready
	// Rgw object exists and pods are running
	//
	rgwPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "rook-ceph-rgw-my-store-a-5fd6fb4489-xv65v",
		Namespace: namespace,
		Labels:    map[string]string{k8sutil.AppAttr: appName, "rgw": "my-store"}}}

	// Get the updated object.
	logger.Info("STARTING PHASE 5")
	// Create RGW pod
	err = r.client.Create(context.TODO(), rgwPod)
	assert.NoError(t, err)

	// Mock client
	newMultisiteAdminOpsCtxFunc = func(objContext *cephobject.Context, spec *cephv1.ObjectStoreSpec) (*cephobject.AdminOpsContext, error) {
		mockClient := &cephobject.MockClient{
			MockDo: func(req *http.Request) (*http.Response, error) {
				if req.URL.RawQuery == "display-name=my-user&format=json&uid=my-user" && req.Method == http.MethodGet && req.URL.Path == "rook-ceph-rgw-my-store.mycluster.svc/admin/user" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCreateJSON))),
					}, nil
				}
				return nil, fmt.Errorf("unexpected request: %q. method %q. path %q", req.URL.RawQuery, req.Method, req.URL.Path)
			},
		}

		context, err := cephobject.NewMultisiteContext(r.context, r.clusterInfo, cephObjectStore)
		assert.NoError(t, err)
		adminClient, err := admin.New("rook-ceph-rgw-my-store.mycluster.svc", "53S6B9S809NUP19IJ2K3", "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR", mockClient)
		assert.NoError(t, err)

		return &cephobject.AdminOpsContext{
			Context:               *context,
			AdminOpsUserAccessKey: "53S6B9S809NUP19IJ2K3",
			AdminOpsUserSecretKey: "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR",
			AdminOpsClient:        adminClient,
		}, nil
	}

	// Run reconcile
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.False(t, res.Requeue)
	err = r.client.Get(context.TODO(), req.NamespacedName, objectUser)
	assert.NoError(t, err)
	assert.Equal(t, "Ready", objectUser.Status.Phase, objectUser)
	logger.Info("PHASE 5 DONE")
}

func TestBuildUpdateStatusInfo(t *testing.T) {
	cephObjectStoreUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
	}

	statusInfo := generateStatusInfo(cephObjectStoreUser)
	assert.NotEmpty(t, statusInfo["secretName"])
	assert.Equal(t, "rook-ceph-object-user-my-store-my-user", statusInfo["secretName"])
}
