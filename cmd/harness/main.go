// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package main

import (
	"fmt"
	"os"

	"github.com/lifan-builds/quotaswitch/internal/harness"
)

func main() {
	app, err := harness.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
	os.Exit(app.Run(os.Args[1:]))
}
