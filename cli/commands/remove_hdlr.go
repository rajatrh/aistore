// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that remove entities from the cluster.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	removeCmdsFlags = map[string][]cli.Flag{
		subcmdRemoveBucket:   {},
		subcmdRemoveObject:   baseLstRngFlags,
		subcmdRemoveNode:     {},
		subcmdRemoveDownload: {},
		subcmdRemoveDsort:    {},
	}

	removeCmds = []cli.Command{
		{
			Name:  commandRemove,
			Usage: "remove entities from the cluster",
			Subcommands: []cli.Command{
				{
					Name:         subcmdRemoveBucket,
					Usage:        "remove ais buckets",
					ArgsUsage:    bucketsArgument,
					Flags:        removeCmdsFlags[subcmdRemoveBucket],
					Action:       removeBucketHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, true /* multiple */, false /* separator */, cmn.ProviderAIS),
				},
				{
					Name:         subcmdRemoveObject,
					Usage:        "remove an object from the bucket",
					ArgsUsage:    optionalObjectsArgument,
					Flags:        removeCmdsFlags[subcmdRemoveObject],
					Action:       removeObjectHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, true /* multiple */, true /* separator */),
				},
				{
					Name:         subcmdRemoveNode,
					Usage:        "remove node from the cluster",
					ArgsUsage:    daemonIDArgument,
					Flags:        removeCmdsFlags[subcmdRemoveNode],
					Action:       removeNodeHandler,
					BashComplete: daemonCompletions(false /* optional */, false /* omit proxies */),
				},
				{
					Name:         subcmdRemoveDownload,
					Usage:        "remove finished download job with given id from the list",
					ArgsUsage:    jobIDArgument,
					Flags:        removeCmdsFlags[subcmdRemoveDownload],
					Action:       removeDownloadHandler,
					BashComplete: downloadIDFinishedCompletions,
				},
				{
					Name:         subcmdRemoveDsort,
					Usage:        fmt.Sprintf("remove finished %s job with given id from the list", cmn.DSortName),
					ArgsUsage:    jobIDArgument,
					Flags:        removeCmdsFlags[subcmdRemoveDsort],
					Action:       removeDsortHandler,
					BashComplete: dsortIDFinishedCompletions,
				},
			},
		},
	}
)

func removeBucketHandler(c *cli.Context) (err error) {
	var buckets []cmn.Bck

	if buckets, err = bucketsFromArgsOrEnv(c); err != nil {
		return
	}
	if err := validateOnlyLocalBuckets(buckets); err != nil {
		return err
	}

	return destroyBuckets(c, buckets)
}

func removeObjectHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}

	// single fullObjName provided. Either remove one object or listFlag/rangeFlag provided
	if c.NArg() == 1 {
		bck, objName := parseBckObjectURI(c.Args().First())
		if bck, err = validateBucket(c, bck, "", false); err != nil {
			return
		}

		if flagIsSet(c, listFlag) || flagIsSet(c, rangeFlag) {
			// list or range operation on a given bucket
			return listOrRangeOp(c, commandRemove, bck)
		}

		if objName == "" {
			return incorrectUsageMsg(c, "%s or %s flag not set with a single bucket argument", listFlag.Name, rangeFlag.Name)
		}

		// ais rm BUCKET/OBJECT_NAME - pass, multiObjOp will handle it
	}

	// list and range flags are invalid with object argument(s)
	if flagIsSet(c, listFlag) || flagIsSet(c, rangeFlag) {
		return incorrectUsageMsg(c, "flags %s are invalid when object names have been provided", strings.Join([]string{listFlag.Name, rangeFlag.Name}, ","))
	}

	// object argument(s) given by the user; operation on given object(s)
	return multiObjOp(c, commandRemove)
}

func removeNodeHandler(c *cli.Context) (err error) {
	daemonID := c.Args().First()
	return clusterRemoveNode(c, daemonID)
}

func removeDownloadHandler(c *cli.Context) (err error) {
	id := c.Args().First()

	if c.NArg() < 1 {
		return missingArgumentsError(c, "download job ID")
	}

	if err = api.DownloadRemove(defaultAPIParams, id); err != nil {
		return
	}

	fmt.Fprintf(c.App.Writer, "download job with id %s successfully removed\n", id)
	return
}

func removeDsortHandler(c *cli.Context) (err error) {
	id := c.Args().First()

	if c.NArg() < 1 {
		return missingArgumentsError(c, cmn.DSortName+" job ID")
	}

	if err = api.RemoveDSort(defaultAPIParams, id); err != nil {
		return
	}

	fmt.Fprintf(c.App.Writer, "%s job with id %s successfully removed\n", cmn.DSortName, id)
	return
}
