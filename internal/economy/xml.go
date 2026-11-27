package economy

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxXMLBytes = 16 << 20

// XMLDocument retains the XML token stream so unknown CE elements, attributes,
// comments and ordering survive round-trips.
type XMLDocument struct {
	Tokens []xml.Token
}

func ParseXML(source string) (*XMLDocument, error) {
	if strings.TrimSpace(source) == "" {
		return nil, errors.New("XML document is empty")
	}
	if len(source) > maxXMLBytes {
		return nil, fmt.Errorf("XML document exceeds %d MiB", maxXMLBytes>>20)
	}
	dec := xml.NewDecoder(strings.NewReader(source))
	dec.Strict = true
	doc := &XMLDocument{}
	depth, roots := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF { break }
		if err != nil { return nil, fmt.Errorf("invalid XML: %w", err) }
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 { roots++ }
			depth++
			doc.Tokens = append(doc.Tokens, xml.CopyToken(t))
		case xml.EndElement:
			depth--
			if depth < 0 { return nil, errors.New("invalid XML: unexpected closing element") }
			doc.Tokens = append(doc.Tokens, xml.CopyToken(t))
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, errors.New("invalid XML: text outside root element")
			}
			doc.Tokens = append(doc.Tokens, xml.CopyToken(t))
		default:
			doc.Tokens = append(doc.Tokens, xml.CopyToken(tok))
		}
	}
	if depth != 0 { return nil, errors.New("invalid XML: unclosed element") }
	if roots != 1 { return nil, fmt.Errorf("invalid XML: expected one root element, found %d", roots) }
	return doc, nil
}

func (d *XMLDocument) Bytes() ([]byte, error) {
	if d == nil || len(d.Tokens) == 0 { return nil, errors.New("XML document has no content") }
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)
	for _, tok := range d.Tokens {
		if err := enc.EncodeToken(tok); err != nil { return nil, err }
	}
	if err := enc.Flush(); err != nil { return nil, err }
	return out.Bytes(), nil
}

func ValidateXML(path, source string) ValidationResult {
	result := ValidationResult{Valid: true, Issues: []Issue{}}
	if strings.TrimSpace(path) == "" {
		result.Valid = false
		result.Issues = append(result.Issues, Issue{Severity: SeverityError, Code: "file.path.empty", Message: "A mission-relative file path is required."})
	}
	if _, err := ParseXML(source); err != nil {
		result.Valid = false
		result.Issues = append(result.Issues, Issue{Severity: SeverityError, Code: "xml.invalid", Message: err.Error(), File: path})
	}
	return result
}
