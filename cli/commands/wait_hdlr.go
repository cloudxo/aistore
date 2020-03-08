// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that wait for specific task.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	waitCmdsFlags = map[string][]cli.Flag{
		subcmdWaitXaction: {
			refreshFlag,
		},
		subcmdWaitDownload: {
			refreshFlag,
		},
		subcmdWaitDSort: {
			refreshFlag,
		},
	}

	waitCmds = []cli.Command{
		{
			Name:  commandWait,
			Usage: "wait for specific task to finish",
			Subcommands: []cli.Command{
				{
					Name:         subcmdWaitXaction,
					Usage:        "wait for xaction to finish",
					ArgsUsage:    xactionWithOptionalBucketArgument,
					Flags:        waitCmdsFlags[subcmdWaitXaction],
					Action:       waitXactionHandler,
					BashComplete: xactionCompletions,
				},
				{
					Name:         subcmdWaitDownload,
					Usage:        "wait for download to finish",
					ArgsUsage:    jobIDArgument,
					Flags:        waitCmdsFlags[subcmdWaitDownload],
					Action:       waitDownloadHandler,
					BashComplete: downloadIDAllCompletions,
				},
				{
					Name:         subcmdWaitDSort,
					Usage:        fmt.Sprintf("wait for %s to finish", cmn.DSortName),
					ArgsUsage:    jobIDArgument,
					Flags:        waitCmdsFlags[subcmdWaitDSort],
					Action:       waitDSortHandler,
					BashComplete: dsortIDAllCompletions,
				},
			},
		},
	}
)

func waitXactionHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, "xaction name")
	}

	xactKind, bucketName := c.Args()[0], ""
	if c.NArg() >= 2 {
		bucketName = c.Args()[1]
	}

	if !cmn.IsValidXaction(xactKind) {
		return fmt.Errorf("%q is not a valid xaction", xactKind)
	}

	bck, _ := parseBckObjectURI(bucketName)
	if bck, err = validateBucket(c, bck, "", true); err != nil {
		return err
	}

	if bck.IsEmpty() && cmn.XactType[xactKind] == cmn.XactTypeBck {
		return missingArgumentsError(c, fmt.Sprintf("bucket name for xaction '%s'", xactKind))
	}

	var (
		aborted bool
		refresh = calcRefreshRate(c)
	)
	for {
		stats, err := api.MakeXactGetRequest(defaultAPIParams, bck, xactKind, cmn.ActXactStats, true)
		if err != nil {
			return err
		}

		finished := true
		for _, nodeStats := range stats {
			for _, xactStats := range nodeStats {
				finished = finished && xactStats.Finished()
				aborted = aborted || xactStats.Aborted()
			}
		}
		if aborted || finished {
			break
		}
		time.Sleep(refresh)
	}
	if aborted {
		if bck.IsEmpty() {
			return fmt.Errorf("xaction %q was aborted", xactKind)
		}
		return fmt.Errorf("xaction %q (bck: %s) was aborted", xactKind, bck)
	}
	return nil
}

func waitDownloadHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, "job id")
	}

	var (
		aborted bool
		refresh = calcRefreshRate(c)
		id      = c.Args()[0]
	)
	for {
		resp, err := api.DownloadStatus(defaultAPIParams, id)
		if err != nil {
			return err
		}

		aborted = aborted || resp.Aborted
		if aborted || resp.JobFinished() {
			break
		}
		time.Sleep(refresh)
	}

	if aborted {
		return fmt.Errorf("download job with %s id was aborted", id)
	}
	return nil
}

func waitDSortHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return missingArgumentsError(c, "job id")
	}

	var (
		aborted bool
		refresh = calcRefreshRate(c)
		id      = c.Args()[0]
	)
	for {
		resp, err := api.MetricsDSort(defaultAPIParams, id)
		if err != nil {
			return err
		}

		finished := true
		for _, targetMetrics := range resp {
			aborted = aborted || targetMetrics.Aborted.Load()
			finished = finished && targetMetrics.Creation.Finished
		}
		if aborted || finished {
			break
		}
		time.Sleep(refresh)
	}

	if aborted {
		return fmt.Errorf("dsort job with %s id was aborted", id)
	}
	return nil
}