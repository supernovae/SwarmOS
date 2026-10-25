package agent

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
)

func TestGuidanceDeliveryEncoding(t *testing.T) {
	c := &GuidanceCollector{}
	c.Delivered("")
	c.Delivered(" \n")
	c.Delivered("flag")
	c.Delivered("follow\nup")
	if got := c.Messages(); !reflect.DeepEqual(got, []string{"flag", "follow\nup"}) {
		t.Fatal(got)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(`["flag","follow\nup"]`)))
	meta := GuidanceMetadata(c.Messages(), nil)
	if meta["operator_steered"] != "true" || meta["guidance_count"] != "2" || meta["guidance_sha256"] != want {
		t.Fatal(meta)
	}
	if GuidanceMetadata(nil, nil) != nil {
		t.Fatal("unsteered run got metadata")
	}
	messages := c.Messages()
	messages[0] = "mutated"
	if c.Messages()[0] != "flag" {
		t.Fatal("collector exposed storage")
	}
}
