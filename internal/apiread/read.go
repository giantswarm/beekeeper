// Package apiread mints an audience-bound reader token without exposing it.
package apiread

import (
	"context"
	"fmt"
	"strings"
	"time"

	authentication "k8s.io/api/authentication/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	MinDuration = 10 * time.Minute
	MaxDuration = 15 * time.Minute
)

// Token checks every RBAC binding that applies to the identity before minting.
// A failed or incomplete policy read fails closed. API authorization itself is
// an operator prerequisite: Kubernetes RBAC cannot describe an external API.
func Token(ctx context.Context, c kubernetes.Interface, namespace, account, audience string, duration time.Duration) (string, error) {
	if namespace == "" || account == "" || audience == "" || duration < MinDuration || duration > MaxDuration {
		return "", fmt.Errorf("require namespace, service account, audience and duration between 10m and 15m")
	}
	bindings, err := c.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("cannot inspect cluster role bindings")
	}
	local, err := c.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("cannot inspect role bindings")
	}
	check := func(ref rbac.RoleRef, ns string) error {
		if ref.APIGroup != rbac.GroupName {
			return fmt.Errorf("unknown role API group")
		}
		var rules []rbac.PolicyRule
		switch ref.Kind {
		case "ClusterRole":
			role, e := c.RbacV1().ClusterRoles().Get(ctx, ref.Name, metav1.GetOptions{})
			if e != nil {
				return fmt.Errorf("cannot inspect cluster role")
			}
			rules = role.Rules
			// Aggregation can change the effective grants asynchronously.
			if role.AggregationRule != nil {
				return fmt.Errorf("aggregated role refused")
			}
		case "Role":
			if ns == "" {
				return fmt.Errorf("invalid role binding")
			}
			role, e := c.RbacV1().Roles(ns).Get(ctx, ref.Name, metav1.GetOptions{})
			if e != nil {
				return fmt.Errorf("cannot inspect role")
			}
			rules = role.Rules
		default:
			return fmt.Errorf("unknown role kind")
		}
		for _, rule := range rules {
			for _, verb := range rule.Verbs {
				if verb != "get" && verb != "list" && verb != "watch" && (verb != "create" || !selfReview(rule)) {
					return fmt.Errorf("service account has non-read permissions")
				}
			}
			for _, resource := range rule.Resources {
				if resource == "*" || resource == "secrets" || strings.Contains(resource, "/") || strings.Contains(resource, "token") {
					return fmt.Errorf("service account can read credentials")
				}
			}
		}
		return nil
	}
	for _, b := range bindings.Items {
		if applies(b.Subjects, namespace, account) {
			if err := check(b.RoleRef, ""); err != nil {
				return "", err
			}
		}
	}
	for _, b := range local.Items {
		if applies(b.Subjects, namespace, account) {
			if err := check(b.RoleRef, b.Namespace); err != nil {
				return "", err
			}
		}
	}
	seconds := int64(duration / time.Second)
	result, err := c.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, account, &authentication.TokenRequest{Spec: authentication.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token request failed")
	}
	if result.Status.Token == "" || !result.Status.ExpirationTimestamp.After(time.Now()) || result.Status.ExpirationTimestamp.After(time.Now().Add(duration)) {
		return "", fmt.Errorf("issuer returned an empty or out-of-bounds token")
	}
	return result.Status.Token, nil
}

func applies(subjects []rbac.Subject, ns, account string) bool {
	for _, s := range subjects {
		switch s.Kind {
		case "ServiceAccount":
			if s.Namespace == ns && s.Name == account {
				return true
			}
		case "User":
			if s.Name == "system:serviceaccount:"+ns+":"+account {
				return true
			}
		case "Group":
			if s.Name == "system:authenticated" || s.Name == "system:serviceaccounts" || s.Name == "system:serviceaccounts:"+ns {
				return true
			}
		}
	}
	return false
}

// Kubernetes grants authenticated identities these virtual review POSTs by
// default. They inspect the caller's permissions and persist no resource.
func selfReview(rule rbac.PolicyRule) bool {
	if len(rule.NonResourceURLs) != 0 || len(rule.APIGroups) != 1 || len(rule.Resources) == 0 {
		return false
	}
	for _, resource := range rule.Resources {
		switch rule.APIGroups[0] {
		case "authorization.k8s.io":
			if resource != "selfsubjectaccessreviews" && resource != "selfsubjectrulesreviews" {
				return false
			}
		case "authentication.k8s.io":
			if resource != "selfsubjectreviews" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
