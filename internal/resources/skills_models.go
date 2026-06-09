package resources

import "strings"

type workspaceLookupItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (w workspaceLookupItem) IsActive() bool {
	return w.Status == "" || strings.EqualFold(w.Status, "active")
}
