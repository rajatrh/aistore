// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands related to specific (not supported for other entities) object actions.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	objectSpecificCmdsFlags = map[string][]cli.Flag{
		commandPrefetch: baseLstRngFlags,
		commandEvict:    baseLstRngFlags,
		commandGet: {
			offsetFlag,
			lengthFlag,
			checksumFlag,
			isCachedFlag,
		},
		commandPut: {
			recursiveFlag,
			trimPrefixFlag,
			concurrencyFlag,
			refreshFlag,
			verboseFlag,
			yesFlag,
			dryRunFlag,
		},
		commandPromote: {
			recursiveFlag,
			overwriteFlag,
			trimPrefixFlag,
			targetFlag,
			verboseFlag,
		},
		commandConcat: {
			verboseFlag,
			recursiveFlag,
		},
		commandCat: {
			offsetFlag,
			lengthFlag,
			checksumFlag,
		},
	}

	objectSpecificCmds = []cli.Command{
		{
			Name:         commandPrefetch,
			Usage:        "prefetch objects from cloud buckets",
			ArgsUsage:    bucketArgument,
			Flags:        objectSpecificCmdsFlags[commandPrefetch],
			Action:       prefetchHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, true /* multiple */, false /* separator */, cmn.Cloud),
		},
		{
			Name:         commandEvict,
			Usage:        "evict objects from the cache",
			ArgsUsage:    optionalObjectsArgument,
			Flags:        objectSpecificCmdsFlags[commandEvict],
			Action:       evictHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, true /* multiple */, true /* separator */, cmn.Cloud),
		},
		{
			Name:         commandGet,
			Usage:        "get the object from the specified bucket",
			ArgsUsage:    getObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandGet],
			Action:       getHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, true /* separator */),
		},
		{
			Name:         commandPut,
			Usage:        "put the objects to the specified bucket",
			ArgsUsage:    putPromoteObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandPut],
			Action:       putHandler,
			BashComplete: putPromoteObjectCompletions,
		},
		{
			Name:         commandPromote,
			Usage:        "promote AIStore-local files and directories to objects",
			ArgsUsage:    putPromoteObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandPromote],
			Action:       promoteHandler,
			BashComplete: putPromoteObjectCompletions,
		},
		{
			Name:      commandConcat,
			Usage:     "concatenate multiple files one by one into new, single object to the specified bucket",
			ArgsUsage: concatObjectArgument,
			Flags:     objectSpecificCmdsFlags[commandConcat],
			Action:    concatHandler,
		},
		{
			Name:         commandCat,
			Usage:        "gets object from the specified bucket and prints it to STDOUT; alias for ais get BUCKET_NAME/OBJECT_NAME -",
			ArgsUsage:    objectArgument,
			Flags:        objectSpecificCmdsFlags[commandCat],
			Action:       catHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, true /* separator */),
		},
	}
)

func prefetchHandler(c *cli.Context) (err error) {
	var (
		bck cmn.Bck
	)

	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}
	if c.NArg() > 1 {
		return incorrectUsageMsg(c, "too many arguments")
	}

	bck, objectName := parseBckObjectURI(c.Args().First())
	if cmn.IsProviderAIS(bck) {
		return fmt.Errorf("prefetch command doesn't support local buckets")
	}
	if bck, err = validateBucket(c, bck, "", false); err != nil {
		return
	}
	//FIXME: it can be easily handled
	if objectName != "" {
		return incorrectUsageMsg(c, "object name not supported, use list flag or range flag")
	}

	if flagIsSet(c, listFlag) || flagIsSet(c, rangeFlag) {
		return listOrRangeOp(c, commandPrefetch, bck)
	}

	return missingArgumentsError(c, "object list or range")
}

func evictHandler(c *cli.Context) (err error) {
	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}

	// default bucket or bucket argument given by the user
	if c.NArg() == 1 {
		bck, objName := parseBckObjectURI(c.Args().First())
		if cmn.IsProviderAIS(bck) {
			return fmt.Errorf("evict command doesn't support local buckets")
		}

		if bck, err = validateBucket(c, bck, "", false); err != nil {
			return
		}

		if flagIsSet(c, listFlag) || flagIsSet(c, rangeFlag) {
			if objName != "" {
				return incorrectUsageMsg(c, "object name (%s) not supported when list or range flag provided", objName)
			}
			// list or range operation on a given bucket
			return listOrRangeOp(c, commandEvict, bck)
		}

		if objName == "" {
			// operation on a given bucket
			return evictBucket(c, bck)
		}

		// evict single object from cloud bucket - multiObjOp will handle
	}

	// list and range flags are invalid with object argument(s)
	if flagIsSet(c, listFlag) || flagIsSet(c, rangeFlag) {
		return incorrectUsageMsg(c, "flags %s are invalid when object names have been provided", strings.Join([]string{listFlag.Name, rangeFlag.Name}, ","))
	}

	// object argument(s) given by the user; operation on given object(s)
	return multiObjOp(c, commandEvict)
}

func getHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		objName     string
		fullObjName = c.Args().Get(0) // empty string if arg not given
		outFile     = c.Args().Get(1) // empty string if arg not given
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "object name in the form bucket/object", "output file")
	}
	if c.NArg() < 2 && !flagIsSet(c, isCachedFlag) {
		return missingArgumentsError(c, "output file")
	}
	bck, objName = parseBckObjectURI(fullObjName)
	if bck, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}
	if objName == "" {
		return incorrectUsageMsg(c, "'%s': missing object name", fullObjName)
	}
	return getObject(c, bck, objName, outFile)
}

func putHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		objName     string
		fileName    = c.Args().Get(0)
		fullObjName = c.Args().Get(1)
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "file to upload", "object name in the form bucket/[object]")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/[object]")
	}
	bck, objName = parseBckObjectURI(fullObjName)

	if bck, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}

	return putObject(c, bck, objName, fileName)
}

func concatHandler(c *cli.Context) (err error) {
	var (
		bck     cmn.Bck
		objName string
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "at least one file to upload", "object name in the form bucket/[object]")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/object")
	}

	fullObjName := c.Args().Get(len(c.Args()) - 1)
	fileNames := make([]string, len(c.Args())-1)
	for i := 0; i < len(c.Args())-1; i++ {
		fileNames[i] = c.Args().Get(i)
	}

	bck, objName = parseBckObjectURI(fullObjName)
	if objName == "" {
		return fmt.Errorf("object name is required")
	}
	if bck, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}

	return concatObject(c, bck, objName, fileNames)
}

func promoteHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		objName     string
		fqn         = c.Args().Get(0)
		fullObjName = c.Args().Get(1)
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "file|directory to promote")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/[object]")
	}

	bck, objName = parseBckObjectURI(fullObjName)
	if bck, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}
	return promoteFileOrDir(c, bck, objName, fqn)
}

func catHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		objName     string
		fullObjName = c.Args().Get(0) // empty string if arg not given
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "object name in the form bucket/object", "output file")
	}
	if c.NArg() > 1 {
		return incorrectUsageError(c, fmt.Errorf("too many arguments"))
	}

	bck, objName = parseBckObjectURI(fullObjName)
	if bck, err = validateBucket(c, bck, fullObjName, false /* optional */); err != nil {
		return
	}
	if objName == "" {
		return incorrectUsageMsg(c, "%q: missing object name", fullObjName)
	}
	return getObject(c, bck, objName, fileStdIO)
}
