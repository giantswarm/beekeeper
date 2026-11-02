package guard

import (
	"encoding/json"
	"testing"
)

const (
	expandsPermissions    = "Expands the Organization permissions section"
	typedDisclosureAction = "type"
)

func TestPermissionDisclosureHook(t *testing.T) {
	for name, tc := range map[string]struct {
		page     string
		action   string
		words    string
		apps     []App
		decision string
	}{
		"expands without a record":                   {permissionsPage, leftClick, expandsPermissions, nil, Allow},
		"expands without naming the recorded change": {permissionsPage, leftClick, "Expand Organization permissions section", changeRecorded, Allow},
		"collapses":                       {permissionsPage, leftClick, "Collapses the Repository permissions section.", nil, Allow},
		"tab":                             {permissionsPage, leftClick, "Switches to the Account permissions tab", nil, Allow},
		"accept is a grant":               {permissionsPage, leftClick, acceptsNew, nil, Deny},
		"save without record":             {permissionsPage, leftClick, savesChangesWord, nil, Deny},
		"save with record":                {permissionsPage, leftClick, savesChangesWord, changeRecorded, Allow},
		"mixed disclosure and save":       {permissionsPage, leftClick, "Expands the Organization permissions section and clicks Save changes", nil, Deny},
		"mixed disclosure and level":      {permissionsPage, leftClick, "Expands the Organization permissions section and selects Read and write", changeRecorded, Deny},
		"a button is not a section":       {permissionsPage, leftClick, "Expands the Save changes button", nil, Deny},
		"a dropdown is not a section":     {permissionsPage, leftClick, "Expands the Members dropdown", nil, Deny},
		"right click is not a disclosure": {permissionsPage, "right_click", expandsPermissions, nil, Deny},
		"typed text is not a disclosure":  {permissionsPage, typedDisclosureAction, expandsPermissions, nil, Deny},
		"keypress is not a disclosure":    {permissionsPage, actionKeyPress, expandsPermissions, nil, Deny},
		"consent button remains guarded":  {otherAppPage, leftClick, expandsPermissions, nil, Deny},
		"another host":                    {lanConsentPage, leftClick, expandsPermissions, nil, Deny},
	} {
		t.Run(name, func(t *testing.T) {
			input := tabInput(tc.action, map[string]any{summaryKey: tc.words})
			ev := toolEvent(browserTools+chromeComputer, input)
			ev["transcript_path"] = transcriptWith(t, Page{URL: tc.page, Title: permissionsTitle}, false)
			d := decideEvent(t, Hook{Apps: func() []App { return tc.apps }}, ev)
			if d == nil || d.PermissionDecision != tc.decision {
				t.Fatalf("decision = %+v, want %s", d, tc.decision)
			}
		})
	}
}

func TestPermissionDisclosureBatchStillChecksSubmit(t *testing.T) {
	disclosure := tabInput(leftClick, map[string]any{summaryKey: expandsPermissions})
	save := tabInput(leftClick, map[string]any{summaryKey: savesChangesWord})
	ev := toolEvent(browserTools+chromeBatch, map[string]any{actionsKey: []any{
		batchAction(chromeComputer, disclosure), batchAction(chromeComputer, save),
	}})
	ev["transcript_path"] = transcriptWith(t, Page{URL: permissionsPage, Title: permissionsTitle}, false)
	d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
	if d == nil || d.PermissionDecision != Deny {
		t.Fatalf("batch decision = %+v, want denial of unrecorded Save", d)
	}
	// Setting a form value is not a disclosure, whatever its summary says.
	ev = toolEvent(browserTools+chromeForm, map[string]any{tabKey: json.Number(tab7), valueKey: "read-and-write", summaryKey: expandsPermissions})
	ev["transcript_path"] = transcriptWith(t, Page{URL: permissionsPage, Title: permissionsTitle}, false)
	d = decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
	if d == nil || d.PermissionDecision != Deny {
		t.Fatalf("form decision = %+v, want denial", d)
	}
}

func TestPermissionDisclosureRequiresDrawnPage(t *testing.T) {
	for _, verdict := range []string{NotDrawn, ""} {
		t.Run(verdict, func(t *testing.T) {
			calls := []drawsCall{{chromeNavigation, map[string]any{urlKey: permissionsPage, tabKey: 7}, "Opened permissions page", permissionsPage}}
			if verdict != "" {
				calls = append(calls, drawResult(verdict, permissionsPage))
			}
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{summaryKey: expandsPermissions}))
			ev["transcript_path"] = drawsTranscript(t, calls...)
			d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
			if d == nil || d.PermissionDecision != Deny {
				t.Fatalf("decision = %+v, want an undrawn page refused", d)
			}
		})
	}
}
