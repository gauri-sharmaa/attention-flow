package source

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func TestParseDump(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`de OpenAI 50 0
en OpenAI 120 0
en.m OpenAI 300 0
en OpenAI_(disambiguation) 7 0
en.m Bitcoin 41 0
en Unrelated 999 0
en.wikibooks OpenAI 5 0
`))
	gz.Close()
	got, err := ParseDump(&buf, map[string]int{"OpenAI": 0, "Bitcoin": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 420 || got[1] != 41 || len(got) != 2 {
		t.Errorf("got %v, want OpenAI 420 (desktop+mobile only), Bitcoin 41", got)
	}
}
