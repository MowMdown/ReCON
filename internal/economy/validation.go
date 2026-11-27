package economy

import "strings"

// ValidateFile currently checks XML well-formedness. Central Economy
// cross-file and domain rules will be layered on after discovery and typed
// models are implemented.
func ValidateFile(path, source string) ValidationResult {
	result := ValidateXML(path, source)
	if result.Valid && strings.TrimSpace(source) == "" {
		result.Valid = false
		result.Issues = append(result.Issues, Issue{Severity: SeverityError, Code: "xml.empty", Message: "The file contains no XML content.", File: path})
	}
	return result
}
