package main

import (
	"math"
	"testing"
	"time"
)

// The F-D1 fix changed `computeDecay`'s second parameter from
// wall-clock daysSinceAccess to runtime-secondsSinceAccess. These tests
// pin the new contract: pass values in SECONDS (86400 per day), and the
// function converts internally.
//
// `now` is captured at the top of each test so the monotonic-time
// reference is consistent across the call.

func TestComputeDecay_LTM(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)

	// 10 runtime-days = 10 * 86400 seconds
	decay := computeDecay(10.0, 10.0*86400.0, true, &created, false, now)
	expected := 10.0 * 0.01
	if !floatEquals(decay, expected) {
		t.Errorf("LTM 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_Weight10Plus(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)

	decay := computeDecay(10.0, 10.0*86400.0, false, &created, false, now)
	expected := 10.0 * 0.02
	if !floatEquals(decay, expected) {
		t.Errorf("weight>=10 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_Weight5Plus(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)

	decay := computeDecay(7.0, 10.0*86400.0, false, &created, false, now)
	expected := 10.0 * 0.05
	if !floatEquals(decay, expected) {
		t.Errorf("weight>=5 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_LowWeight_Fresh(t *testing.T) {
	now := time.Now()
	created := now

	// 3 runtime-days, ageFactor = 0/30 = 0, baseDecay = 0.1
	decay := computeDecay(3.0, 3.0*86400.0, false, &created, false, now)
	// 3 days * 0.1 = 0.3
	if !floatEquals(decay, 0.3) {
		t.Errorf("low-weight fresh 3-day decay: got %v, want 0.3", decay)
	}
}

func TestComputeDecay_LowWeight_Old(t *testing.T) {
	now := time.Now()
	created := now.Add(-60 * 24 * time.Hour)

	// 10 runtime-days, ageFactor = 1.0, baseDecay = 0.3
	decay := computeDecay(3.0, 10.0*86400.0, false, &created, false, now)
	// 10 days * 0.3 = 3.0
	if !floatEquals(decay, 3.0) {
		t.Errorf("low-weight old 10-day decay: got %v, want 3.0", decay)
	}
}

func TestComputeDecay_Aggressive(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)

	decay := computeDecay(7.0, 10.0*86400.0, false, &created, true, now)
	expected := 10.0 * 0.05 * 2.0
	if !floatEquals(decay, expected) {
		t.Errorf("aggressive 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_NoCreatedAt(t *testing.T) {
	now := time.Now()
	decay := computeDecay(3.0, 10.0*86400.0, false, nil, false, now)
	if !floatEquals(decay, 3.0) {
		t.Errorf("no createdAt 10-day decay: got %v, want 3.0", decay)
	}
}

// floatEquals compares two floats with epsilon tolerance
func floatEquals(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}