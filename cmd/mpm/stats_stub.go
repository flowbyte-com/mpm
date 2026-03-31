//go:build !808

package main

// Placeholder versions for standard build (no achievement tracking)

// BadgeID is empty type in standard build
type BadgeID string

// Badge is empty in standard build
type Badge struct{}

// TrophyRoom is empty in standard build
type TrophyRoom struct{}

// InitTrophies does nothing in standard build
func InitTrophies(basePath string) {}

// AwardBadge does nothing in standard build
func AwardBadge(id BadgeID) {}

// AwardBadgeRandom does nothing in standard build
func AwardBadgeRandom(possible ...BadgeID) {}
