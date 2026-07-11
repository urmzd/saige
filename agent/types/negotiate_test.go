package types

import "testing"

func TestContentSupportSupports(t *testing.T) {
	cs := ContentSupport{NativeTypes: map[MediaType]bool{
		MediaPNG: true,
		MediaPDF: true,
		MediaCSV: false,
	}}
	if !cs.Supports(MediaPNG) || !cs.Supports(MediaPDF) {
		t.Error("declared native types must report supported")
	}
	if cs.Supports(MediaCSV) || cs.Supports(MediaMP4) {
		t.Error("undeclared or false types must report unsupported")
	}
}

func TestContentSupportZeroValue(t *testing.T) {
	// A provider without ContentNegotiator yields the zero value: nothing native,
	// so every file goes through the extraction fallback.
	var cs ContentSupport
	if cs.Supports(MediaPDF) {
		t.Error("zero-value ContentSupport must support nothing")
	}
}
