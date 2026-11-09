package plugin

import (
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

func TestUnitKeys(t *testing.T) {
	for k, want := range map[string]bool{
		"image": true, "image:1024x1024": true, "image:1024x1024:hd": true, "audio_second": true, "1k_chars": true,
		"image:": false, "image::hd": false, "image:a:b:c": false, "token": false, "": false,
	} {
		if ValidUnit(k) != want {
			t.Errorf("ValidUnit(%q) = %v", k, !want)
		}
	}
	u := map[string]int64{"image": 1, "image:1024x1024": 2, "image:1024x1024:hd": 3}
	for _, tc := range []struct {
		size, quality string
		want          int64
	}{{"1024x1024", "hd", 3}, {"1024x1024", "standard", 2}, {"512x512", "hd", 1}, {"", "", 1}} {
		if got := ImageRate(u, tc.size, tc.quality); got != tc.want {
			t.Errorf("ImageRate(%s, %s) = %d", tc.size, tc.quality, got)
		}
	}
	r := Rates{Input: 10, Units: map[string]int64{"image": 3}}.Scale(1.5)
	if r.Input != 15 || r.Units["image"] != 5 { // 4.5 rounds half up
		t.Errorf("scaled %+v", r)
	}
}

func TestUnitCharge(t *testing.T) {
	r := Rates{Input: 1_000_000, Units: map[string]int64{UnitSearch: 2_000, UnitAudioSecond: 3, UnitImage: 40_000}}
	// 2 search units, 1.5 s of audio (4.5, half up), one image; tokens are not counted.
	u := ir.Usage{Input: 1000, SearchUnits: 2, AudioMs: 1500, Images: 1}
	if got := r.UnitCharge(u); got != 4_000+5+40_000 {
		t.Errorf("UnitCharge = %d", got)
	}
	if got := (Rates{Input: 1}).UnitCharge(u); got != 0 {
		t.Errorf("no unit rates: %d", got)
	}
}
