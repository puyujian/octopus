package model

// WebSearchCall describes an observed server-side search, never a pending
// client function call. Source lists may arrive after the call completes.
type WebSearchCall struct {
	ID     string           `json:"id"`
	Status string           `json:"status,omitempty"`
	Action *WebSearchAction `json:"action,omitempty"`
}

type WebSearchAction struct {
	Type    string            `json:"type,omitempty"`
	Query   string            `json:"query,omitempty"`
	Queries []string          `json:"queries,omitempty"`
	Sources []WebSearchSource `json:"sources,omitempty"`
}

type WebSearchSource struct {
	Type  string `json:"type,omitempty"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

func MergeWebSearchCalls(calls []WebSearchCall, update WebSearchCall) []WebSearchCall {
	if update.ID == "" {
		return calls
	}
	index := -1
	for i := range calls {
		if calls[i].ID == update.ID {
			index = i
			break
		}
	}
	if index < 0 {
		calls = append(calls, WebSearchCall{ID: update.ID})
		index = len(calls) - 1
	}
	call := &calls[index]
	currentIsTerminal := call.Status != "" && call.Status != "in_progress" && call.Status != "searching"
	updateIsProgress := update.Status == "in_progress" || update.Status == "searching"
	if update.Status != "" && (!currentIsTerminal || !updateIsProgress) {
		call.Status = update.Status
	}
	if update.Action != nil {
		if call.Action == nil {
			call.Action = &WebSearchAction{}
		} else {
			copied := *call.Action
			call.Action = &copied
		}
		if update.Action.Type != "" {
			call.Action.Type = update.Action.Type
		}
		if update.Action.Query != "" {
			call.Action.Query = update.Action.Query
		}
		if len(update.Action.Queries) > 0 {
			call.Action.Queries = append([]string(nil), update.Action.Queries...)
		}
		if update.Action.Sources != nil {
			sources := make([]WebSearchSource, len(call.Action.Sources))
			copy(sources, call.Action.Sources)
			for _, source := range update.Action.Sources {
				if source.URL == "" {
					continue
				}
				found := false
				for i := range sources {
					if sources[i].URL == source.URL {
						if sources[i].Title == "" {
							sources[i].Title = source.Title
						}
						found = true
						break
					}
				}
				if !found {
					sources = append(sources, source)
				}
			}
			call.Action.Sources = sources
		}
	}
	return calls
}
