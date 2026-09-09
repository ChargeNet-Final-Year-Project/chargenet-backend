// Package emulator implements VoltGo, ChargeSync and EcoWatt as independently
// authenticated simulated operators under /emulator/{slug}/ocpi. Each reads its
// own seeded network and exchanges commands, telemetry and CDRs over loopback HTTP.
// The emulators serve OCPI 2.3.0.
package emulator

import "strings"

// Profile is one emulated CPO's identity and the dev credentials the seed data
// links it to. The tokens here are the same constants
// scripts/init.sql writes onto the cpo_partners row, so they are the values both
// sides authenticate with out of the box.
type Profile struct {
	// Slug is the URL segment the emulator is mounted under:
	// /emulator/{slug}/ocpi.
	Slug string
	// CPOID is the cpo_partners.id the emulator serves.
	CPOID string
	// PartyID and CountryCode are the emulator's OCPI identity (e.g. MY/VLT).
	PartyID     string
	CountryCode string
	// Name is rendered in the CPO business_details of its credentials.
	Name string
	// TokenB is what ChargeNet presents to call the emulator; TokenC is what the
	// emulator presents to call ChargeNet.
	TokenB string
	TokenC string
}

// Profiles are the three emulators, in the order the platform seeds them.
var Profiles = []Profile{
	{Slug: "voltgo", CPOID: "cpo_voltgo", PartyID: "VLT", CountryCode: "MY", Name: "VoltGo",
		TokenB: "voltgo-token-b-dev", TokenC: "voltgo-token-c-dev"},
	{Slug: "chargesync", CPOID: "cpo_chargesync", PartyID: "CSY", CountryCode: "MY", Name: "ChargeSync",
		TokenB: "chargesync-token-b-dev", TokenC: "chargesync-token-c-dev"},
	{Slug: "ecowatt", CPOID: "cpo_ecowatt", PartyID: "ECO", CountryCode: "MY", Name: "EcoWatt",
		TokenB: "ecowatt-token-b-dev", TokenC: "ecowatt-token-c-dev"},
}

// ProfileBySlug finds a profile by its URL segment, case-insensitively.
func ProfileBySlug(slug string) (Profile, bool) {
	for _, p := range Profiles {
		if strings.EqualFold(p.Slug, slug) {
			return p, true
		}
	}
	return Profile{}, false
}

// basePath is the OCPI root the emulator is mounted under.
func (p Profile) basePath() string { return "/emulator/" + p.Slug + "/ocpi" }
