package contract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestImportedPlanDocument_ExactBytesAndRefusals(t *testing.T) {
	content := "# Existing plan\r\nPreserve café and original tests.\r\n"
	doc := ImportedPlanDocument{Name: "plan.md", Content: content, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	if err := ValidateImportedPlanDocument(&doc); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(doc)
	var restored ImportedPlanDocument
	if err := json.Unmarshal(body, &restored); err != nil || restored != doc {
		t.Fatalf("import changed source bytes: %+v, %v", restored, err)
	}
	for name, text := range map[string]string{"empty": " ", "nul": "plan\x00", "binary": string([]byte{0xff}), "oversize": strings.Repeat("x", MaxImportedPlanBytes+1), "modified": content + "changed"} {
		t.Run(name, func(t *testing.T) {
			bad := doc
			bad.Content = text
			if name != "modified" {
				bad.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
			}
			if err := ValidateImportedPlanDocument(&bad); err == nil {
				t.Fatal("invalid imported document accepted")
			}
		})
	}
}
