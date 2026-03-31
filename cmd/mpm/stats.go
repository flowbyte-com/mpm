//go:build 808

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BadgeID identifies an achievement
type BadgeID string

const (
	BadgeFirstMemory    BadgeID = "first_memory"
	BadgeCleanSlate     BadgeID = "clean_slate"
	BadgeSystemSurgeon  BadgeID = "system_surgeon"
	BadgeDeepSleeper    BadgeID = "deep_sleeper"
	BadgeShutdownMaster BadgeID = "shutdown_master"
	BadgeDoctorRun      BadgeID = "doctor_run"
	BadgeRebootRecovery BadgeID = "reboot_recovery"
	BadgeModeMaster     BadgeID = "mode_master"
	BadgePersonaSwitch  BadgeID = "persona_switch"
	BadgeHackMaster     BadgeID = "hack_master"
	BadgeCrustafarian   BadgeID = "crustafarian"
	BadgeDoctorPerfect  BadgeID = "doctor_perfect"
)

// Badge represents an achievement
type Badge struct {
	ID          BadgeID `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
	UnlockedAt  string  `json:"unlocked_at,omitempty"`
	Count       int     `json:"count,omitempty"`
}

// TrophyRoom tracks achievements
type TrophyRoom struct {
	Path         string         `json:"-"`
	Unlocked     map[BadgeID]Badge `json:"unlocked"`
	TotalActions int               `json:"total_actions"`
	FirstSeen    string          `json:"first_seen"`
	Mutex       sync.Mutex       `json:"-"`
}

var badgeDefinitions = map[BadgeID]Badge{
	BadgeFirstMemory:    {ID: BadgeFirstMemory, Name: "First Memory", Description: "Stored your first session", Icon: "🧠"},
	BadgeCleanSlate:     {ID: BadgeCleanSlate, Name: "Clean Slate", Description: "Ran shred for the first time", Icon: "🧹"},
	BadgeSystemSurgeon:  {ID: BadgeSystemSurgeon, Name: "System Surgeon", Description: "Ran doctor --fix to repair", Icon: "🏥"},
	BadgeDeepSleeper:    {ID: BadgeDeepSleeper, Name: "Deep Sleeper", Description: "Daemon ran for 7+ days", Icon: "😴"},
	BadgeShutdownMaster: {ID: BadgeShutdownMaster, Name: "Shutdown Master", Description: "Graceful shutdown 10 times", Icon: "🛑"},
	BadgeDoctorRun:      {ID: BadgeDoctorRun, Name: "Health Inspector", Description: "Ran mpm doctor", Icon: "👨‍⚕️"},
	BadgeRebootRecovery: {ID: BadgeRebootRecovery, Name: "Phoenix", Description: "Rebooted and recovered", Icon: "🔥"},
	BadgeModeMaster:     {ID: BadgeModeMaster, Name: "Mode Master", Description: "Activated 5 different modes", Icon: "🎛️"},
	BadgePersonaSwitch:  {ID: BadgePersonaSwitch, Name: "Chameleon", Description: "Switched 3 personas", Icon: "🦎"},
	BadgeHackMaster:     {ID: BadgeHackMaster, Name: "Hack Master", Description: "Full shred + rebuild cycle", Icon: "⚡"},
	BadgeCrustafarian:   {ID: BadgeCrustafarian, Name: "True Believer", Description: "Read SOUL.md", Icon: "🦞"},
	BadgeDoctorPerfect:  {ID: BadgeDoctorPerfect, Name: "Perfect Health", Description: "Doctor with 0 warnings", Icon: "💯"},
}

var trophies *TrophyRoom

// InitTrophies initializes the trophy room
func InitTrophies(basePath string) {
	trophies = &TrophyRoom{
		Path:     filepath.Join(basePath, ".trophies.json"),
		Unlocked: make(map[BadgeID]Badge),
	}
	trophies.Load()
}

func (t *TrophyRoom) Load() {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	
	data, err := os.ReadFile(t.Path)
	if err != nil {
		t.FirstSeen = time.Now().Format(time.RFC3339)
		return
	}
	json.Unmarshal(data, t)
}

func (t *TrophyRoom) Save() {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	
	data, _ := json.MarshalIndent(t, "", "  ")
	os.WriteFile(t.Path, data, 0644)
}

// Unlock awards a badge
func (t *TrophyRoom) Unlock(id BadgeID) bool {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	
	if _, exists := t.Unlocked[id]; exists {
		return false
	}
	
	badge := badgeDefinitions[id]
	badge.UnlockedAt = time.Now().Format(time.RFC3339)
	t.Unlocked[id] = badge
	t.TotalActions++
	t.Save()
	
	fmt.Printf("\n🦞 ★ NEW ACHIEVEMENT ★ %s\n", lobsterTiny)
	fmt.Printf("   %s %s\n", badge.Icon, badge.Name)
	fmt.Printf("   %s\n\n", badge.Description)
	
	return true
}

// Has checks if badge is unlocked
func (t *TrophyRoom) Has(id BadgeID) bool {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	_, exists := t.Unlocked[id]
	return exists
}

// GetUnlocked returns all unlocked badges
func (t *TrophyRoom) GetUnlocked() []Badge {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	result := make([]Badge, 0, len(t.Unlocked))
	for _, b := range t.Unlocked {
		result = append(result, b)
	}
	return result
}

// PrintTrophies displays the trophy room
func (t *TrophyRoom) PrintTrophies() {
	unlocked := t.GetUnlocked()
	total := len(badgeDefinitions)
	
	fmt.Printf("\n🏆 TROPHY ROOM (%d/%d)\n\n", len(unlocked), total)
	
	if len(unlocked) == 0 {
		fmt.Println("   No achievements yet. Keep using mpm!")
		return
	}
	
	for _, b := range unlocked {
		fmt.Printf("   %s %s ★\n", b.Icon, b.Name)
		fmt.Printf("      %s\n", b.Description)
	}
	
	locked := total - len(unlocked)
	if locked > 0 {
		fmt.Printf("\n   %d locked badges to earn!\n", locked)
	}
	fmt.Println()
}

// AwardBadge is the global award function
func AwardBadge(id BadgeID) {
	if trophies != nil {
		trophies.Unlock(id)
	}
}

// AwardBadgeRandom awards one of provided badges randomly
func AwardBadgeRandom(possible ...BadgeID) {
	if trophies != nil && len(possible) > 0 {
		r := rand.Intn(len(possible))
		trophies.Unlock(possible[r])
	}
}
