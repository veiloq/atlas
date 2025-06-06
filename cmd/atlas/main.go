// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/veiloq/atlas/pkg/cmdapi"
	"github.com/veiloq/atlas/pkg/cmdapi/vercheck"
	"github.com/veiloq/atlas/pkg/cmdlog"
	_ "github.com/veiloq/atlas/pkg/docker"
	_ "github.com/veiloq/atlas/sql/mysql"
	_ "github.com/veiloq/atlas/sql/mysql/mysqlcheck"
	_ "github.com/veiloq/atlas/sql/postgres"
	_ "github.com/veiloq/atlas/sql/postgres/postgrescheck"
	_ "github.com/veiloq/atlas/sql/sqlite"
	_ "github.com/veiloq/atlas/sql/sqlite/sqlitecheck"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/mattn/go-isatty"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
	"golang.org/x/mod/semver"
)

func main() {
	cmdapi.Root.SetOut(os.Stdout)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// On first signal seen, cancel the context. On the second signal, force stop immediately.
		stop := make(chan os.Signal, 2)
		defer close(stop)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(stop)
		<-stop   // wait for first interrupt
		cancel() // cancel context to gracefully stop
		fmt.Fprintln(cmdapi.Root.OutOrStdout(), "\ninterrupt received, wait for exit or ^C to terminate")
		// Wait for the context to be canceled. Issuing a second interrupt will cause the process to force stop.
		<-stop // will not block if no signal received due to main routine exiting
		os.Exit(1)
	}()
	ctx, err := extendContext(ctx)
	cobra.CheckErr(err)
	ctx, done := initialize(ctx)
	update := checkForUpdate(ctx)
	err = cmdapi.Root.ExecuteContext(ctx)
	if u := update(); u != "" {
		_ = cmdlog.WarnOnce(os.Stderr, cmdlog.ColorCyan(u))
	}
	done(err)
	if err != nil {
		os.Exit(1)
	}
}

const (
	// envNoUpdate when enabled it cancels checking for update
	envNoUpdate = "ATLAS_NO_UPDATE_NOTIFIER"
	vercheckURL = "https://vercheck.ariga.io"
)

func noText() string { return "" }

func checkForUpdate(ctx context.Context) func() string {
	version := cmdapi.Version()
	// Users may skip update checking behavior.
	if v := os.Getenv(envNoUpdate); v != "" {
		return noText
	}
	// Skip if the current binary version isn't set (dev mode).
	if !semver.IsValid(version) {
		return noText
	}
	endpoint := vercheckEndpoint(ctx)
	vc := vercheck.New(endpoint)
	if isatty.IsTerminal(os.Stdout.Fd()) {
		return bgCheck(ctx, version, vc)
	}
	return func() string {
		msg, _ := runCheck(ctx, vc, version)
		return msg
	}
}

// bgCheck checks for version updates and security advisories for Atlas in the background.
func bgCheck(ctx context.Context, version string, vc *vercheck.VerChecker) func() string {
	done := make(chan struct{})
	var message string
	go func() {
		defer close(done)
		msg, err := runCheck(ctx, vc, version)
		if err != nil {
			return
		}
		message = msg
	}()
	return func() string {
		select {
		case <-done:
		case <-time.After(time.Millisecond * 500):
		}
		return message
	}
}

func runCheck(ctx context.Context, vc *vercheck.VerChecker, version string) (string, error) {
	payload, err := vc.Check(ctx, version)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := vercheck.Notify.Execute(&b, payload); err != nil {
		return "", err
	}
	return b.String(), nil
}
