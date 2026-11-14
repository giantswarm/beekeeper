package wake

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Fixtures: repositories and the wake refs of their objects.
const (
	repoKlaus   = "giantswarm/klaus"
	repoMuster  = "giantswarm/muster"
	repoRoadmap = "giantswarm/roadmap"

	issueRoadmap100 = "issue:giantswarm/roadmap#100"
	issueRoadmap101 = "issue:giantswarm/roadmap#101"
	prKlaus55       = "pr:giantswarm/klaus#55"
	ciMusterM1      = "ci:giantswarm/muster@m1"
)

func TestMap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		event   string
		body    string
		want    Delivery
		wantErr bool
	}{
		{
			name:  "board item edited",
			event: eventProjectsV2Item,
			body: `{"action":"edited","projects_v2_item":{"node_id":"PVTI_giantswarm_273_giantswarm/roadmap#101",
				"project_node_id":"PVT_giantswarm_273","content_node_id":"I_giantswarm/roadmap#101","content_type":"Issue"},
				"changes":{"field_value":{"field_name":"Status","to":{"name":"Up Next ➡️"}}}}`,
			want: Delivery{Event: eventProjectsV2Item, Action: "edited",
				Refs:        []string{"board:PVTI_giantswarm_273_giantswarm/roadmap#101"},
				ProjectNode: "PVT_giantswarm_273", BoardItem: "PVTI_giantswarm_273_giantswarm/roadmap#101", ContentType: "Issue"},
		},
		{
			name:  "board item reordered is noise",
			event: eventProjectsV2Item,
			body:  `{"action":"reordered","projects_v2_item":{"node_id":"PVTI_x","project_node_id":"PVT_x"}}`,
			want: Delivery{Event: eventProjectsV2Item, Action: "reordered", ProjectNode: "PVT_x", BoardItem: "PVTI_x",
				Ignore: true},
		},
		{
			name:    "board item without node id",
			event:   eventProjectsV2Item,
			body:    `{"action":"edited","projects_v2_item":{}}`,
			wantErr: true,
		},
		{
			name:  "issue closed",
			event: eventIssues,
			body:  `{"action":"closed","issue":{"number":100},"repository":{"full_name":"giantswarm/roadmap"}}`,
			want: Delivery{Event: eventIssues, Action: "closed", Repo: repoRoadmap,
				Refs: []string{issueRoadmap100}},
		},
		{
			name:  "issue repo from its repository_url",
			event: eventIssues,
			body:  `{"action":"edited","issue":{"number":7,"repository_url":"https://api.github.com/repos/giantswarm/klaus"}}`,
			want:  Delivery{Event: eventIssues, Action: "edited", Refs: []string{"issue:giantswarm/klaus#7"}},
		},
		{
			name:  "sub-issue added wakes parent and child",
			event: eventSubIssues,
			body: `{"action":"sub_issue_added","parent_issue":{"number":90},"parent_issue_repo":{"full_name":"giantswarm/roadmap"},
				"sub_issue":{"number":101,"repository_url":"https://api.github.com/repos/giantswarm/roadmap"},
				"repository":{"full_name":"giantswarm/roadmap"}}`,
			want: Delivery{Event: eventSubIssues, Action: "sub_issue_added", Repo: repoRoadmap,
				Refs: []string{issueRoadmap101, "issue:giantswarm/roadmap#90"}},
		},
		{
			name:  "dependency added wakes blocked and blocking",
			event: eventIssueDependencies,
			body: `{"action":"blocked_by_added","blocked_issue":{"number":101},"blocking_issue":{"number":100,
				"html_url":"https://github.com/giantswarm/other/issues/100"},"repository":{"full_name":"giantswarm/roadmap"}}`,
			want: Delivery{Event: eventIssueDependencies, Action: "blocked_by_added", Repo: repoRoadmap,
				Refs: []string{"issue:giantswarm/other#100", issueRoadmap101}},
		},
		{
			name:  "pull request opened may link",
			event: eventPullRequest,
			body:  `{"action":"opened","pull_request":{"number":55,"head":{"sha":"a1"}},"repository":{"full_name":"giantswarm/klaus"}}`,
			want: Delivery{Event: eventPullRequest, Action: "opened", Repo: repoKlaus,
				Refs: []string{prKlaus55}, LinkPR: "giantswarm/klaus#55"},
		},
		{
			name:  "pull request synchronize doesn't link",
			event: eventPullRequest,
			body:  `{"action":"synchronize","pull_request":{"number":55},"repository":{"full_name":"giantswarm/klaus"}}`,
			want: Delivery{Event: eventPullRequest, Action: "synchronize", Repo: repoKlaus,
				Refs: []string{prKlaus55}},
		},
		{
			name:  "pull request repo from its base",
			event: eventPullRequest,
			body:  `{"action":"closed","pull_request":{"number":3,"base":{"repo":{"full_name":"alice/x"}}}}`,
			want:  Delivery{Event: eventPullRequest, Action: "closed", Repo: "alice/x", Refs: []string{"pr:alice/x#3"}},
		},
		{
			name:    "pull request without number",
			event:   eventPullRequest,
			body:    `{"action":"closed","pull_request":{}}`,
			wantErr: true,
		},
		{
			name:  "review submitted",
			event: eventPullRequestReview,
			body:  `{"action":"submitted","pull_request":{"number":55},"repository":{"full_name":"giantswarm/klaus"}}`,
			want: Delivery{Event: eventPullRequestReview, Action: "submitted", Repo: repoKlaus,
				Refs: []string{prKlaus55}},
		},
		{
			name:  "check run completed",
			event: eventCheckRun,
			body: `{"action":"completed","check_run":{"head_sha":"a1","conclusion":"success","pull_requests":[{"number":55}]},
				"repository":{"full_name":"giantswarm/klaus"}}`,
			want: Delivery{Event: eventCheckRun, Action: "completed", Repo: repoKlaus,
				Refs: []string{"ci:giantswarm/klaus@a1"}},
		},
		{
			name:  "check suite completed",
			event: eventCheckSuite,
			body:  `{"action":"completed","check_suite":{"head_sha":"b2"},"repository":{"full_name":"giantswarm/klaus"}}`,
			want: Delivery{Event: eventCheckSuite, Action: "completed", Repo: repoKlaus,
				Refs: []string{"ci:giantswarm/klaus@b2"}},
		},
		{
			name:  "commit status",
			event: eventStatus,
			body:  `{"sha":"f9","state":"success","context":"ci","repository":{"full_name":"giantswarm/klaus"}}`,
			want:  Delivery{Event: eventStatus, Repo: repoKlaus, Refs: []string{"ci:giantswarm/klaus@f9"}},
		},
		{
			name:  "push to main",
			event: eventPush,
			body:  `{"ref":"refs/heads/main","after":"m1","repository":{"full_name":"giantswarm/muster"}}`,
			want:  Delivery{Event: eventPush, Repo: repoMuster, Refs: []string{ciMusterM1}},
		},
		{
			name:  "branch deleted",
			event: eventPush,
			body:  `{"ref":"refs/heads/x","after":"0000000000000000000000000000000000000000","repository":{"full_name":"giantswarm/muster"}}`,
			want:  Delivery{Event: eventPush, Repo: repoMuster},
		},
		{
			name:  "installation repositories",
			event: eventInstallationRepos,
			body:  `{"action":"added","repositories_added":[{"full_name":"giantswarm/happa"}],"repositories_removed":[]}`,
			want: Delivery{Event: eventInstallationRepos, Action: "added", InstallationRepos: true,
				ReposAdded: []string{"giantswarm/happa"}},
		},
		{
			name:  "ping is ignored",
			event: "ping",
			body:  `{"zen":"x"}`,
			want:  Delivery{Event: "ping", Ignore: true},
		},
		{
			name:  "unknown event is ignored unread",
			event: "issue_comment",
			body:  `not json`,
			want:  Delivery{Event: "issue_comment", Ignore: true},
		},
		{
			name:    "malformed subscribed payload",
			event:   eventIssues,
			body:    `{`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Map(tt.event, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Map() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestObjectRefs(t *testing.T) {
	t.Parallel()
	item := &v1alpha1.Item{Status: v1alpha1.ItemStatus{
		Issue: &v1alpha1.ItemIssue{Repo: repoRoadmap, Number: 101},
		Board: &v1alpha1.ItemBoard{NodeID: "PVTI_1"},
		Wait:  &v1alpha1.Wait{On: v1alpha1.WaitOnDependency, Ref: issueRoadmap100},
	}}
	if got, want := ItemRefs(item), []string{"board:PVTI_1", issueRoadmap100, issueRoadmap101}; !reflect.DeepEqual(got, want) {
		t.Errorf("ItemRefs = %v, want %v", got, want)
	}
	if got := ItemRefs(&v1alpha1.Item{}); got != nil {
		t.Errorf("ItemRefs(empty) = %v, want nil", got)
	}

	change := &v1alpha1.Change{Status: v1alpha1.ChangeStatus{
		PR:   &v1alpha1.ChangePR{Repo: repoMuster, Number: 12, HeadSHA: "a1", MergeSHA: "m1"},
		Wait: &v1alpha1.Wait{On: v1alpha1.WaitOnCI, Ref: ciMusterM1, Since: metav1.Now()},
	}}
	if got, want := ChangeRefs(change), []string{"ci:giantswarm/muster@a1", ciMusterM1, "pr:giantswarm/muster#12"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangeRefs = %v, want %v", got, want)
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref, kind, value string
		ok               bool
	}{
		{"issue:giantswarm/roadmap#1", "issue", "giantswarm/roadmap#1", true},
		{"pr:a/b#2", "pr", "a/b#2", true},
		{"ci:a/b@sha", "ci", "a/b@sha", true},
		{"board:PVTI_x", "board", "PVTI_x", true},
		{"other:x", "", "", false},
	}
	for _, tt := range tests {
		kind, value, ok := Split(tt.ref)
		if kind != tt.kind || value != tt.value || ok != tt.ok {
			t.Errorf("Split(%q) = %q, %q, %v", tt.ref, kind, value, ok)
		}
	}
	if repo, sha, ok := SplitCI("ci:a/b@c1"); repo != "a/b" || sha != "c1" || !ok {
		t.Errorf("SplitCI = %q %q %v", repo, sha, ok)
	}
	if _, _, ok := SplitCI("ci:a/b"); ok {
		t.Error("SplitCI without sha is ok")
	}
}
