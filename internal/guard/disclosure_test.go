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
		"collapses":                       {permissionsPage, leftClick, "Collapses the Organization permissions section.", nil, Allow},
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
			input := tabInput(tc.action, map[string]any{summaryKey: tc.words, refKey: refTwelve})
			ev := toolEvent(browserTools+chromeComputer, input)
			control := disclosureButton
			if name == "tab" {
				control = `- tab "Account permissions" [ref_12]`
			}
			ev["transcript_path"] = disclosureTranscript(t, tc.page, control)
			d := decideEvent(t, Hook{Apps: func() []App { return tc.apps }}, ev)
			if d == nil || d.PermissionDecision != tc.decision {
				t.Fatalf("decision = %+v, want %s", d, tc.decision)
			}
		})
	}
}

func TestPermissionDisclosureBatchStillChecksSubmit(t *testing.T) {
	disclosure := tabInput(leftClick, map[string]any{summaryKey: expandsPermissions, refKey: refTwelve})
	save := tabInput(leftClick, map[string]any{summaryKey: savesChangesWord})
	ev := toolEvent(browserTools+chromeBatch, map[string]any{actionsKey: []any{
		batchAction(chromeComputer, disclosure), batchAction(chromeComputer, save),
	}})
	ev["transcript_path"] = disclosureTranscript(t, permissionsPage, disclosureButton)
	d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
	if d == nil || d.PermissionDecision != Deny {
		t.Fatalf("batch decision = %+v, want denial of unrecorded Save", d)
	}
	// Setting a form value is not a disclosure, whatever its summary says.
	ev = toolEvent(browserTools+chromeForm, map[string]any{tabKey: json.Number(tab7), valueKey: "read-and-write", summaryKey: expandsPermissions})
	ev["transcript_path"] = disclosureTranscript(t, permissionsPage, disclosureButton)
	d = decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
	if d == nil || d.PermissionDecision != Deny {
		t.Fatalf("form decision = %+v, want denial", d)
	}
}

func TestPermissionDisclosureRequiresDrawnPage(t *testing.T) {
	for _, verdict := range []string{NotDrawn, ""} {
		t.Run(verdict, func(t *testing.T) {
			calls := []drawsCall{disclosureRead(permissionsPage, disclosureButton)}
			if verdict != "" {
				calls = append(calls, drawResult(verdict, permissionsPage))
			}
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{summaryKey: expandsPermissions, refKey: refTwelve}))
			ev["transcript_path"] = drawsTranscript(t, calls...)
			d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
			if d == nil || d.PermissionDecision != Deny {
				t.Fatalf("decision = %+v, want an undrawn page refused", d)
			}
		})
	}
}

const disclosureButton = `- button "Organization permissions" [ref_12]`

func disclosureRead(page, text string) drawsCall {
	return drawsCall{chromeReadPage, map[string]any{tabKey: 7}, text, page}
}

func disclosureTranscript(t *testing.T, page, control string) string {
	t.Helper()
	return drawsTranscript(t, disclosureRead(page, control), drawResult(Drawn, page))
}

func TestPermissionDisclosureChecksActualRef(t *testing.T) {
	for name, tc := range map[string]struct {
		control  string
		ref      string
		decision string
	}{
		"the actual disclosure":                 {disclosureButton, refTwelve, Allow},
		"find format":                           {`- ref_12: button "Organization permissions"`, refTwelve, Allow},
		"save disguised by summary":             {`- button "Save changes" [ref_12]`, refTwelve, Deny},
		"accept disguised by summary":           {`- button "Accept new permissions" [ref_12]`, refTwelve, Deny},
		"permission level disguised by summary": {`- option "Read and write" [ref_12]`, refTwelve, Deny},
		"permission dropdown":                   {`- combobox "Organization permissions" [ref_12]`, refTwelve, Deny},
		"a different section":                   {`- button "Repository permissions" [ref_12]`, refTwelve, Deny},
		"no ref":                                {disclosureButton, "", Deny},
		"unknown ref":                           {disclosureButton, "ref_123", Deny},
		"no page read":                          {"", refTwelve, Deny},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{refKey: tc.ref, summaryKey: expandsPermissions}))
			ev["transcript_path"] = disclosureTranscript(t, permissionsPage, tc.control)
			d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
			if d == nil || d.PermissionDecision != tc.decision {
				t.Fatalf("decision = %+v, want %s", d, tc.decision)
			}
		})
	}
}

func TestPermissionDisclosureDiscardsStaleEvidence(t *testing.T) {
	read := disclosureRead(permissionsPage, disclosureButton)
	otherTab := read
	otherTab.input = map[string]any{tabKey: 8}
	for name, calls := range map[string][]drawsCall{
		"different tab":         {otherTab},
		"different URL":         {disclosureRead(otherAppsPage, disclosureButton)},
		"latest read says Save": {read, disclosureRead(permissionsPage, `- button "Save changes" [ref_12]`)},
		"navigation after read": {read, {chromeNavigation, map[string]any{urlKey: permissionsPage, tabKey: 7}, "Navigated back", permissionsPage}},
		"click after read":      {read, {chromeComputer, tabInput(leftClick, nil), "Clicked", permissionsPage}},
		"unrelated tool result": {{chromePageText, map[string]any{tabKey: 7}, disclosureButton, permissionsPage}},
	} {
		t.Run(name, func(t *testing.T) {
			calls = append(calls, drawResult(Drawn, permissionsPage))
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{refKey: refTwelve, summaryKey: expandsPermissions}))
			ev["transcript_path"] = drawsTranscript(t, calls...)
			d := decideEvent(t, Hook{Apps: func() []App { return nil }}, ev)
			if d == nil || d.PermissionDecision != Deny {
				t.Fatalf("decision = %+v, want stale/foreign evidence refused", d)
			}
		})
	}
}

func TestPermissionDisclosureDoesNotReuseRefsAfterBatchNavigation(t *testing.T) {
	ev := toolEvent(browserTools+chromeBatch, map[string]any{actionsKey: []any{
		batchAction(chromeNavigation, map[string]any{urlKey: permissionsPage, tabKey: 7}),
		batchAction(chromeComputer, tabInput(leftClick, map[string]any{refKey: refTwelve, summaryKey: expandsPermissions})),
	}})
	ev["transcript_path"] = disclosureTranscript(t, permissionsPage, disclosureButton)
	d := decideEvent(t, Hook{Apps: func() []App { return changeRecorded }}, ev)
	if d == nil || d.PermissionDecision != Deny {
		t.Fatalf("decision = %+v, want evidence invalidated by batch navigation", d)
	}
}
