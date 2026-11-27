// Package economy implements ReCON's server-side DayZ Central Economy editor.
//
// The economy workspace belongs to one ReCON installation (one DayZ server).
// Parsing, editing, validation, persistence and generation live here;
// recon-mod is reserved for mission-file transport.
package economy

import "time"

type File struct {
	Path        string    `json:"path"`
	SourceHash  string    `json:"sourceHash"`
	WorkingHash string    `json:"workingHash"`
	HasChanges  bool      `json:"hasChanges"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

type Issue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	File     string   `json:"file,omitempty"`
	Element  string   `json:"element,omitempty"`
}

type ValidationResult struct {
	Valid  bool    `json:"valid"`
	Issues []Issue `json:"issues"`
}

type WorkspaceStatus struct {
	Files           int  `json:"files"`
	Changed         int  `json:"changed"`
	Ready           bool `json:"ready"`
	RestartRequired bool `json:"restartRequired"`
}
