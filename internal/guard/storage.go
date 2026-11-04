package guard

import "regexp"

// BrowseStorageBarrier identifies a refusal without repeating code that might
// contain credentials in the reason.
const BrowseStorageBarrier = "Refused: browse storage and session token barrier."

// These names cover ordinary property and bracket access as well as aliases
// assigned from these APIs. This is a conservative check, not a JS sandbox.
var browsePrivateAPI = regexp.MustCompile(`(?i)localstorage|sessionstorage|indexeddb|cookies?|session[_-]?tokens?|access[_-]?tokens?|authorization|\bfetch\b|XMLHttpRequest|sendBeacon|\beval\b|\bFunction\b`)

// storageRefusal applies only to browse turns. UI scripts still work, but
// storage APIs and direct requests cannot bypass the visible UI. Inspect every
// batch action before any of them executes.
func storageRefusal(ev event, shared func() ([]string, bool)) string {
	if shared == nil || !isBrowserTool(ev.ToolName) {
		return ""
	}
	if _, browse := shared(); !browse {
		return ""
	}
	var forbidden func(string, map[string]any) bool
	forbidden = func(tool string, input map[string]any) bool {
		switch chromeTool(tool) {
		case chromeScript:
			code, _ := input[textKey].(string)
			return browsePrivateAPI.MatchString(code)
		case chromeBatch:
			for _, a := range batchActions(input) {
				if forbidden(a.name, a.input) {
					return true
				}
			}
		}
		return false
	}
	if forbidden(ev.ToolName, ev.ToolInput) {
		return BrowseStorageBarrier + " Never read page storage (localStorage, sessionStorage, cookies, IndexedDB) or session tokens, or call a site's API with them. Work through the visible UI only; report this barrier and any steps not completed."
	}
	return ""
}
