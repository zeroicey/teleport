// Package aidoc serves the machine-facing usage guide ("how to use this site")
// as plain Markdown, so an AI agent can read it and act on it without executing
// JavaScript.
//
// Why this is a Go package and not a page in the Vue app: an agent that fetches
// a URL receives only the HTML the server sends. The SPA renders client-side, so
// its routes return a shell with no content. Anything an agent must be able to
// read has to be server-rendered — that is the whole reason this exists.
//
// The document is stored as an embedded .md file rather than a Go string so it
// can be edited as prose. Placeholders ({{APP_BASE}}, ...) are substituted from
// the live Config at request time, so a change to the deployment prefix or host
// cannot leave the guide describing the old one.
package aidoc

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zeroicey/teleport/backend/internal/config"
)

//go:embed guide.md
var guideTemplate string

// Placeholders the guide may use. Kept deliberately small and explicit: an
// unrecognised {{...}} in the guide is a bug, and TestNoUnresolvedPlaceholders
// fails the build rather than shipping a guide that tells an agent to call
// "{{APP_BASE}}/api/reports".
const (
	phAppBase         = "{{APP_BASE}}"
	phRoutePrefix     = "{{ROUTE_PREFIX}}"
	phDefaultShareHrs = "{{DEFAULT_SHARE_HOURS}}"
	phMaxContent      = "{{MAX_CONTENT_BYTES}}"
	phEnvironment     = "{{ENVIRONMENT}}"
	phKeyAppTTL       = "{{KEY_APPLICATION_TTL}}"
	phKeyClaimWindow  = "{{KEY_CLAIM_WINDOW}}"
	phKeyApplyPerHour = "{{KEY_APPLY_PER_HOUR}}"
)

// Facts are the live deployment values substituted into the guide.
type Facts struct {
	AppBase           string
	RoutePrefix       string
	DefaultShareHours int
	MaxContentBytes   int64
	Environment       string
	// The key-application limits are surfaced too: they are what tells a polling
	// agent how long it may wait, how long it has to claim, and when it is being
	// rate-limited, and all three change with configuration.
	KeyApplicationTTL time.Duration
	KeyClaimWindow    time.Duration
	KeyApplyPerHour   int
}

// FactsFrom extracts the substitution values from a loaded Config.
func FactsFrom(cfg *config.Config) Facts {
	return Facts{
		AppBase:           cfg.AppBaseURL(),
		RoutePrefix:       cfg.RoutePrefix,
		DefaultShareHours: cfg.DefaultShareHours,
		MaxContentBytes:   cfg.MaxContentBytes,
		Environment:       cfg.Environment,
		KeyApplicationTTL: cfg.KeyApplicationTTL,
		KeyClaimWindow:    cfg.KeyClaimWindow,
		KeyApplyPerHour:   cfg.KeyApplyPerHour,
	}
}

// Markdown returns the guide with every placeholder substituted.
func Markdown(f Facts) string {
	return strings.NewReplacer(
		phAppBase, f.AppBase,
		phRoutePrefix, f.RoutePrefix,
		phDefaultShareHrs, strconv.Itoa(f.DefaultShareHours),
		phMaxContent, strconv.FormatInt(f.MaxContentBytes, 10),
		phEnvironment, f.Environment,
		phKeyAppTTL, HumanDuration(f.KeyApplicationTTL),
		phKeyClaimWindow, HumanDuration(f.KeyClaimWindow),
		phKeyApplyPerHour, strconv.Itoa(f.KeyApplyPerHour),
	).Replace(guideTemplate)
}

// HumanDuration renders a duration the way a person writes it: "24h", "30m",
// "45s", "1h30m".
//
// time.Duration.String() would render 24h as "24h0m0s", which reads like a
// machine value pasted into prose and makes the guide harder to skim. Zero and
// negative are reported as 0s rather than "0s" by accident of sign handling.
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	var b strings.Builder

	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= days * 24 * time.Hour
	}
	if hours := d / time.Hour; hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
		d -= hours * time.Hour
	}
	if minutes := d / time.Minute; minutes > 0 {
		fmt.Fprintf(&b, "%dm", minutes)
		d -= minutes * time.Minute
	}
	if seconds := d / time.Second; seconds > 0 {
		fmt.Fprintf(&b, "%ds", seconds)
	}
	return b.String()
}
