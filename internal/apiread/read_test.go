package apiread

import (
	"context"
	"fmt"
	"testing"
	"time"

	authentication "k8s.io/api/authentication/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

const (
	testResource           = "actors"
	testCreate             = "create"
	testAPI                = "api"
	testAccess             = "access"
	testAuthorizationGroup = "authorization.k8s.io"
	testToken              = "private-value"
)

func TestTokenPolicy(t *testing.T) {
	for _, tc := range []struct {
		name             string
		verbs, resources []string
		group            string
		duration         time.Duration
		want             bool
	}{
		{"reader", []string{"get", "list", "watch"}, []string{testResource}, "", 10 * time.Minute, true},
		{"writer", []string{"get", testCreate}, []string{testResource}, "", 10 * time.Minute, false},
		{"wildcard", []string{"*"}, []string{testResource}, "", 10 * time.Minute, false},
		{"exec subresource", []string{"get"}, []string{"pods/exec"}, "", 10 * time.Minute, false},
		{"credential reader", []string{"get"}, []string{"secrets"}, "", 10 * time.Minute, false},
		{"group writer", []string{"patch"}, []string{testResource}, "system:authenticated", 10 * time.Minute, false},
		{"service account group writer", []string{"delete"}, []string{testResource}, "system:serviceaccounts:api", 10 * time.Minute, false},
		{"below issuer minimum", []string{"get"}, []string{testResource}, "", 5 * time.Minute, false},
		{"unbounded", []string{"get"}, []string{testResource}, "", 0, false},
		{"too long", []string{"get"}, []string{testResource}, "", 16 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := rbac.Subject{Kind: "ServiceAccount", Name: "reader", Namespace: testAPI}
			if tc.group != "" {
				subject = rbac.Subject{Kind: "Group", Name: tc.group}
			}
			client := fake.NewClientset(&rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: testAccess}, Rules: []rbac.PolicyRule{{Verbs: tc.verbs, Resources: tc.resources}}}, &rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: testAccess}, Subjects: []rbac.Subject{subject}, RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "ClusterRole", Name: testAccess}})
			// Every authenticated identity inherits this virtual review grant.
			for _, object := range []runtime.Object{
				&rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "basic-user"}, Rules: []rbac.PolicyRule{{APIGroups: []string{testAuthorizationGroup}, Resources: []string{"selfsubjectaccessreviews"}, Verbs: []string{testCreate}}}},
				&rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "basic-user"}, Subjects: []rbac.Subject{{Kind: "Group", Name: "system:authenticated"}}, RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "ClusterRole", Name: "basic-user"}},
			} {
				if err := client.Tracker().Add(object); err != nil {
					t.Fatal(err)
				}
			}
			minted := false
			client.PrependReactor(testCreate, "serviceaccounts", func(action kt.Action) (bool, runtime.Object, error) {
				minted = true
				req := action.(kt.CreateAction).GetObject().(*authentication.TokenRequest)
				if len(req.Spec.Audiences) != 1 || req.Spec.Audiences[0] != testAPI || *req.Spec.ExpirationSeconds != 600 {
					t.Fatal("wrong token scope")
				}
				return true, &authentication.TokenRequest{Status: authentication.TokenRequestStatus{Token: testToken, ExpirationTimestamp: metav1.NewTime(time.Now().Add(599 * time.Second))}}, nil
			})
			token, err := Token(context.Background(), client, testAPI, "reader", testAPI, tc.duration)
			if (err == nil) != tc.want || minted != tc.want {
				t.Fatalf("token minted=%v err=%v", minted, err)
			}
			if tc.want && token != testToken {
				t.Fatal("missing token")
			}
		})
	}
}

func TestTokenFailsClosed(t *testing.T) {
	for _, stage := range []string{"clusterrolebindings", "rolebindings", "serviceaccounts"} {
		t.Run(stage, func(t *testing.T) {
			c := fake.NewClientset()
			c.PrependReactor("*", stage, func(kt.Action) (bool, runtime.Object, error) { return true, nil, fmt.Errorf("sensitive diagnostic") })
			_, err := Token(context.Background(), c, testAPI, "reader", testAPI, 10*time.Minute)
			if err == nil || err.Error() == "sensitive diagnostic" {
				t.Fatal("did not fail closed")
			}
		})
	}
}

func TestTokenRejectsIssuerExtension(t *testing.T) {
	c := fake.NewClientset()
	c.PrependReactor(testCreate, "serviceaccounts", func(kt.Action) (bool, runtime.Object, error) {
		return true, &authentication.TokenRequest{Status: authentication.TokenRequestStatus{Token: testToken, ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))}}, nil
	})
	if token, err := Token(context.Background(), c, testAPI, "reader", testAPI, 10*time.Minute); err == nil || token != "" {
		t.Fatal("accepted extended token")
	}
}

func TestNamespacedWriterAndMissingRoleRefused(t *testing.T) {
	for _, missing := range []bool{false, true} {
		c := fake.NewClientset(&rbac.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: testAccess, Namespace: "other"}, Subjects: []rbac.Subject{{Kind: "ServiceAccount", Name: "reader", Namespace: testAPI}}, RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "Role", Name: "writer"}})
		if !missing {
			_, err := c.RbacV1().Roles("other").Create(context.Background(), &rbac.Role{ObjectMeta: metav1.ObjectMeta{Name: "writer"}, Rules: []rbac.PolicyRule{{Verbs: []string{"update"}, Resources: []string{testResource}}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if token, err := Token(context.Background(), c, testAPI, "reader", testAPI, 10*time.Minute); err == nil || token != "" {
			t.Fatal("accepted namespaced or unresolved write grant")
		}
		for _, action := range c.Actions() {
			if action.GetVerb() == testCreate && action.GetResource().Resource == "serviceaccounts" {
				t.Fatal("minted before refusing")
			}
		}
	}
}

func TestSelfReviewException(t *testing.T) {
	for _, tc := range []struct {
		group, resource string
		want            bool
	}{
		{testAuthorizationGroup, "selfsubjectaccessreviews", true},
		{testAuthorizationGroup, "selfsubjectrulesreviews", true},
		{"authentication.k8s.io", "selfsubjectreviews", true},
		{"authentication.k8s.io", "tokenreviews", false},
		{testAuthorizationGroup, "subjectaccessreviews", false},
		{"custom", "selfsubjectaccessreviews", false},
	} {
		if selfReview(rbac.PolicyRule{APIGroups: []string{tc.group}, Resources: []string{tc.resource}}) != tc.want {
			t.Errorf("incorrect exception for %s/%s", tc.group, tc.resource)
		}
	}
}
