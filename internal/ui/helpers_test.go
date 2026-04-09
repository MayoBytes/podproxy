package ui

import (
	"testing"
	"time"
)

func TestUnixTime_Nil(t *testing.T) {
	if got := unixTime(nil); got != -1 {
		t.Errorf("unixTime(nil): want -1, got %d", got)
	}
}

func TestUnixTime_NonNil(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0)
	if got := unixTime(&ts); got != 1_700_000_000 {
		t.Errorf("unixTime: want 1700000000, got %d", got)
	}
}
