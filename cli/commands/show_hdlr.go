// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file contains implementation of the top-level `show` command.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/stats"

	"github.com/urfave/cli"
)

type (
	daemonTemplateStats struct {
		DaemonID string
		Stats    []*stats.BaseXactStatsExt
	}

	xactionTemplateCtx struct {
		S       *[]daemonTemplateStats
		Verbose bool
	}
)

var (
	showCmdsFlags = map[string][]cli.Flag{
		subcmdShowBucket: {
			providerFlag,
			fastDetailsFlag,
			cachedFlag,
		},
		subcmdShowDisk: append(
			longRunFlags,
			jsonFlag,
			noHeaderFlag,
		),
		subcmdShowDownload: {
			regexFlag,
			progressBarFlag,
			refreshFlag,
			verboseFlag,
		},
		subcmdShowDsort: {
			regexFlag,
			refreshFlag,
			verboseFlag,
			logFlag,
		},
		subcmdShowObject: {
			objPropsFlag,
			noHeaderFlag,
			jsonFlag,
		},
		subcmdShowNode: append(
			longRunFlags,
			jsonFlag,
		),
		subcmdShowXaction: {
			jsonFlag,
			allItemsFlag,
			activeFlag,
			verboseFlag,
		},
		subcmdShowRebalance: {
			refreshFlag,
		},
	}

	showCmds = []cli.Command{
		{
			Name:  commandShow,
			Usage: "show control info about entities in the cluster",
			Subcommands: []cli.Command{
				{
					Name:         subcmdShowBucket,
					Usage:        "show bucket details",
					ArgsUsage:    optionalBucketArgument,
					Flags:        showCmdsFlags[subcmdShowBucket],
					Action:       showBucketHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, false /* separator */),
				},
				{
					Name:         subcmdShowDisk,
					Usage:        "show disk stats for targets",
					ArgsUsage:    optionalTargetIDArgument,
					Flags:        showCmdsFlags[subcmdShowDisk],
					Action:       showDisksHandler,
					BashComplete: daemonCompletions(true /* optional */, true /* omit proxies */),
				},
				{
					Name:         subcmdShowDownload,
					Usage:        "show information about download jobs",
					ArgsUsage:    optionalJobIDArgument,
					Flags:        showCmdsFlags[subcmdShowDownload],
					Action:       showDownloadsHandler,
					BashComplete: downloadIDAllCompletions,
				},
				{
					Name:         subcmdShowDsort,
					Usage:        fmt.Sprintf("show information about %s jobs", cmn.DSortName),
					ArgsUsage:    optionalJobIDArgument,
					Flags:        showCmdsFlags[subcmdShowDsort],
					Action:       showDsortHandler,
					BashComplete: dsortIDAllCompletions,
				},
				{
					Name:         subcmdShowObject,
					Usage:        "show object details",
					ArgsUsage:    objectArgument,
					Flags:        showCmdsFlags[subcmdShowObject],
					Action:       showObjectHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, true /* separator */),
				},
				{
					Name:         subcmdShowNode,
					Usage:        "show node details",
					ArgsUsage:    optionalDaemonIDArgument,
					Flags:        showCmdsFlags[subcmdShowNode],
					Action:       showNodeHandler,
					BashComplete: daemonCompletions(true /* optional */, false /* omit proxies */),
				},
				{
					Name:         subcmdShowXaction,
					Usage:        "show xaction details",
					ArgsUsage:    optionalXactionWithOptionalBucketArgument,
					Description:  xactKindsMsg,
					Flags:        showCmdsFlags[subcmdShowXaction],
					Action:       showXactionHandler,
					BashComplete: xactionCompletions,
				},
				{
					Name:         subcmdShowRebalance,
					Usage:        "show rebalance details",
					ArgsUsage:    noArguments,
					Flags:        showCmdsFlags[subcmdShowRebalance],
					Action:       showRebalanceHandler,
					BashComplete: flagCompletions,
				},
			},
		},
	}
)

func showBucketHandler(c *cli.Context) (err error) {
	bck, objName := parseBckObjectURI(c.Args().First())
	if objName != "" {
		return objectNameArgumentNotSupported(c, objName)
	}
	if bck, err = validateBucket(c, bck, "", true); err != nil {
		return
	}
	return bucketDetails(c, bck)
}

func showDisksHandler(c *cli.Context) (err error) {
	daemonID := c.Args().First()
	if _, err = fillMap(); err != nil {
		return
	}

	if err = updateLongRunParams(c); err != nil {
		return
	}

	return daemonDiskStats(c, daemonID, flagIsSet(c, jsonFlag), flagIsSet(c, noHeaderFlag))
}

func showDownloadsHandler(c *cli.Context) (err error) {
	id := c.Args().First()

	if c.NArg() < 1 { // list all download jobs
		return downloadJobsList(c, parseStrFlag(c, regexFlag))
	}

	// display status of a download job with given id
	return downloadJobStatus(c, id)
}

func showDsortHandler(c *cli.Context) (err error) {
	id := c.Args().First()

	if c.NArg() < 1 { // list all dsort jobs
		return dsortJobsList(c, parseStrFlag(c, regexFlag))
	}

	// display status of a dsort job with given id
	return dsortJobStatus(c, id)
}

func showNodeHandler(c *cli.Context) (err error) {
	daemonID := c.Args().First()
	if _, err = fillMap(); err != nil {
		return
	}

	if err = updateLongRunParams(c); err != nil {
		return
	}

	return daemonStats(c, daemonID, flagIsSet(c, jsonFlag))
}

func showXactionHandler(c *cli.Context) (err error) {
	var (
		bck        cmn.Bck
		objName    string
		xactKind   = c.Args().Get(0) // empty string if no arg given
		bucketName = c.Args().Get(1) // empty string if no arg given
	)

	if bucketName != "" {
		bck, objName = parseBckObjectURI(bucketName)
		if objName != "" {
			return objectNameArgumentNotSupported(c, objName)
		}
		if bck, err = validateBucket(c, bck, "", false); err != nil {
			return
		}
	}

	if xactKind != "" {
		if !cmn.IsValidXaction(xactKind) {
			return fmt.Errorf("%q is not a valid xaction", xactKind)
		}

		// valid xaction
		if cmn.XactType[xactKind] == cmn.XactTypeBck {
			if bck.Name == "" {
				return missingArgumentsError(c, fmt.Sprintf("bucket name for xaction '%s'", xactKind))
			}
		}
	}

	xactStatsMap, err := api.MakeXactGetRequest(defaultAPIParams, bck, xactKind, commandStats, flagIsSet(c, allItemsFlag))
	if err != nil {
		return
	}

	if flagIsSet(c, activeFlag) {
		for daemonID, daemonStats := range xactStatsMap {
			if len(daemonStats) == 0 {
				continue
			}
			runningStats := make([]*stats.BaseXactStatsExt, 0, len(daemonStats))
			for _, xact := range daemonStats {
				if xact.Running() {
					runningStats = append(runningStats, xact)
				}
			}
			xactStatsMap[daemonID] = runningStats
		}
	}
	for daemonID, daemonStats := range xactStatsMap {
		if len(daemonStats) == 0 {
			delete(xactStatsMap, daemonID)
		}
	}

	dts := make([]daemonTemplateStats, len(xactStatsMap))
	i := 0
	for daemonID, daemonStats := range xactStatsMap {
		s := daemonTemplateStats{
			DaemonID: daemonID,
			Stats:    daemonStats,
		}
		dts[i] = s
		i++
	}
	ctx := xactionTemplateCtx{
		S:       &dts,
		Verbose: flagIsSet(c, verboseFlag),
	}

	return templates.DisplayOutput(ctx, c.App.Writer, templates.XactionsBodyTmpl, flagIsSet(c, jsonFlag))
}

func showObjectHandler(c *cli.Context) (err error) {
	var (
		fullObjName = c.Args().Get(0) // empty string if no arg given
	)

	if c.NArg() < 1 {
		return missingArgumentsError(c, "object name in format bucket/object")
	}
	bck, object := parseBckObjectURI(fullObjName)
	if bck, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}
	if object == "" {
		return incorrectUsageMsg(c, "no object specified in '%s'", fullObjName)
	}
	return objectStats(c, bck, object)
}

func showRebalanceHandler(c *cli.Context) (err error) {
	return showGlobalRebalance(c, flagIsSet(c, refreshFlag), calcRefreshRate(c))
}
