/*
Copyright (c) 2026 Artem Kustikov

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestSamplesDecodeStrictly guards config/samples against drift from the API.
func TestSamplesDecodeStrictly(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "docling_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no samples found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var ds DoclingServer
		if err := yaml.UnmarshalStrict(data, &ds); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if ds.Kind != "DoclingServer" || ds.APIVersion != GroupVersion.String() {
			t.Errorf("%s: unexpected kind/apiVersion %s/%s", f, ds.Kind, ds.APIVersion)
		}
	}
}
