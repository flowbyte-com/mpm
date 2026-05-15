package main

import (
	"math"
	"testing"
	"time"
)

func TestComputeDecay_LTM(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)
	
	decay := computeDecay(10.0, 10.0, true, &created, false, now)
	expected := 10.0 * 0.01
	if !floatEquals(decay, expected) {
		t.Errorf("LTM 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_Weight10Plus(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)
	
	decay := computeDecay(10.0, 10.0, false, &created, false, now)
	expected := 10.0 * 0.02
	if !floatEquals(decay, expected) {
		t.Errorf("weight>=10 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_Weight5Plus(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)
	
	decay := computeDecay(7.0, 10.0, false, &created, false, now)
	expected := 10.0 * 0.05
	if !floatEquals(decay, expected) {
		t.Errorf("weight>=5 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_LowWeight_Fresh(t *testing.T) {
	now := time.Now()
	created := now
	
	decay := computeDecay(3.0, 3.0, false, &created, false, now)
	// ageFactor = 0/30 = 0, baseDecay = 0.1
	// 3 days * 0.1 = 0.3
	if !floatEquals(decay, 0.3) {
		t.Errorf("low-weight fresh 3-day decay: got %v, want 0.3", decay)
	}
}

func TestComputeDecay_LowWeight_Old(t *testing.T) {
	now := time.Now()
	created := now.Add(-60 * 24 * time.Hour)
	
	decay := computeDecay(3.0, 10.0, false, &created, false, now)
	// ageFactor = 1.0, baseDecay = 0.3
	// 10 days * 0.3 = 3.0
	if !floatEquals(decay, 3.0) {
		t.Errorf("low-weight old 10-day decay: got %v, want 3.0", decay)
	}
}

func TestComputeDecay_Aggressive(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * 24 * time.Hour)
	
	decay := computeDecay(7.0, 10.0, false, &created, true, now)
	expected := 10.0 * 0.05 * 2.0
	if !floatEquals(decay, expected) {
		t.Errorf("aggressive 10-day decay: got %v, want %v", decay, expected)
	}
}

func TestComputeDecay_NoCreatedAt(t *testing.T) {
	now := time.Now()
	decay := computeDecay(3.0, 10.0, false, nil, false, now)
	if !floatEquals(decay, 3.0) {
		t.Errorf("no createdAt 10-day decay: got %v, want 3.0", decay)
	}
}

// floatEquals compares two floats with epsilon tolerance
func floatEquals(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
