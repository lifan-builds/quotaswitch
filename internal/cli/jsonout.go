// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package cli

const jsonSchemaVersion = 1

type Envelope struct {
	SchemaVersion int       `json:"schema_version"`
	Command       string    `json:"command"`
	Data          any       `json:"data,omitempty"`
	Error         *ErrorDoc `json:"error,omitempty"`
}

type ErrorDoc struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Hint       string      `json:"hint,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

type Candidate struct {
	Number int    `json:"number,omitempty"`
	ID     string `json:"id"`
	Email  string `json:"email,omitempty"`
	Alias  string `json:"alias,omitempty"`
}
