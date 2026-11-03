package main

import (
	"bytes"
	"testing"
)

func TestD3FixturesDeterministic(t *testing.T) {
	dir := t.TempDir()
	a, err := generateFixtures(dir+"/a", dir+"/company-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateFixtures(dir+"/b", dir+"/company-b")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Tender, b.Tender) || !bytes.Equal(a.Correction, b.Correction) {
		t.Fatal("tender/correction bytes differ between generations")
	}
	for key, data := range a.Originals {
		if !bytes.Equal(data, b.Originals[key]) {
			t.Fatalf("original %s differs", key)
		}
	}
}
