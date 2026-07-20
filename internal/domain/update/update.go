package update

import "time"

type Component string

const (
	Self  Component = "self"
	Proxy Component = "proxy"
)

type Release struct {
	Component Component `json:"component"`
	Current   string    `json:"current"`
	Available string    `json:"available"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}
