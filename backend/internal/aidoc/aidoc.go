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
	"strconv"
	"strings"

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
)

// Facts are the live deployment values substituted into the guide.
type Facts struct {
	AppBase           string
	RoutePrefix       string
	DefaultShareHours int
	MaxContentBytes   int64
	Environment       string
}

// FactsFrom extracts the substitution values from a loaded Config.
func FactsFrom(cfg *config.Config) Facts {
	return Facts{
		AppBase:           cfg.AppBaseURL(),
		RoutePrefix:       cfg.RoutePrefix,
		DefaultShareHours: cfg.DefaultShareHours,
		MaxContentBytes:   cfg.MaxContentBytes,
		Environment:       cfg.Environment,
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
	).Replace(guideTemplate)
}
