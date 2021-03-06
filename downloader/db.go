// Package downloader implements functionality to download resources into AIS cluster from external source.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/sdomino/scribble"
)

const (
	persistDownloaderJobsPath = "downloader_jobs.db" // base name to persist downloader jobs' file
	downloaderErrors          = "errors"
	downloaderTasks           = "tasks"

	// Number of errors stored in memory. When the number of errors exceeds
	// this number, then all errors will be flushed to disk
	errCacheSize = 100

	// Number of tasks stored in memory. When the number of tasks exceeds
	// this number, then all errors will be flushed to disk
	taskInfoCacheSize = 1000
)

var (
	errJobNotFound = errors.New("job not found")
)

type downloaderDB struct {
	mtx    sync.RWMutex
	driver *scribble.Driver

	errCache      map[string][]cmn.TaskErrInfo // memory cache for errors, see: errCacheSize
	taskInfoCache map[string][]cmn.TaskDlInfo  // memory cache for tasks, see: taskInfoCacheSize
}

func newDownloadDB() (*downloaderDB, error) {
	config := cmn.GCO.Get()
	driver, err := scribble.New(filepath.Join(config.Confdir, persistDownloaderJobsPath), nil)
	if err != nil {
		return nil, err
	}

	return &downloaderDB{
		driver:        driver,
		errCache:      make(map[string][]cmn.TaskErrInfo, 10),
		taskInfoCache: make(map[string][]cmn.TaskDlInfo, 10),
	}, nil
}

func (db *downloaderDB) errors(id string) (errors []cmn.TaskErrInfo, err error) {
	if err := db.driver.Read(downloaderErrors, id, &errors); err != nil {
		if !os.IsNotExist(err) {
			glog.Error(err)
			return nil, err
		}
		// If there was nothing in DB, return only values in the cache
		return db.errCache[id], nil
	}

	errors = append(errors, db.errCache[id]...)
	return
}

func (db *downloaderDB) getErrors(id string) (errors []cmn.TaskErrInfo, err error) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	return db.errors(id)
}

func (db *downloaderDB) persistError(id, objname string, errMsg string) {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	errInfo := cmn.TaskErrInfo{Name: objname, Err: errMsg}
	if len(db.errCache[id]) < errCacheSize { // if possible store error in cache
		db.errCache[id] = append(db.errCache[id], errInfo)
		return
	}

	errMsgs, err := db.errors(id) // it will also append errors from cache
	if err != nil {
		glog.Error(err)
		return
	}
	errMsgs = append(errMsgs, errInfo)

	if err := db.driver.Write(downloaderErrors, id, errMsgs); err != nil {
		glog.Error(err)
		return
	}

	db.errCache[id] = db.errCache[id][:0] // clear cache
}

func (db *downloaderDB) tasks(id string) (tasks []cmn.TaskDlInfo, err error) {
	if err := db.driver.Read(downloaderTasks, id, &tasks); err != nil {
		if !os.IsNotExist(err) {
			glog.Error(err)
			return nil, err
		}
		// If there was nothing in DB, return empty list
		return db.taskInfoCache[id], nil
	}
	tasks = append(tasks, db.taskInfoCache[id]...)
	return
}

func (db *downloaderDB) persistTaskInfo(id string, task cmn.TaskDlInfo) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if len(db.taskInfoCache[id]) < taskInfoCacheSize { // if possible store task in cache
		db.taskInfoCache[id] = append(db.taskInfoCache[id], task)
		return nil
	}

	persistedTasks, err := db.tasks(id) // it will also append tasks from cache
	if err != nil {
		return err
	}
	persistedTasks = append(persistedTasks, task)

	if err := db.driver.Write(downloaderTasks, id, persistedTasks); err != nil {
		glog.Error(err)
		return err
	}

	db.taskInfoCache[id] = db.taskInfoCache[id][:0] // clear cache
	return nil
}

func (db *downloaderDB) getTasks(id string) (tasks []cmn.TaskDlInfo, err error) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	return db.tasks(id)
}

// flushes caches into the disk
func (db *downloaderDB) flush(id string) error {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	if len(db.errCache[id]) > 0 {
		errMsgs, err := db.errors(id) // it will also append errors from cache
		if err != nil {
			return err
		}

		if err := db.driver.Write(downloaderErrors, id, errMsgs); err != nil {
			glog.Error(err)
			return err
		}

		db.errCache[id] = db.errCache[id][:0] // clear cache
	}

	if len(db.taskInfoCache[id]) > 0 {
		persistedTasks, err := db.tasks(id) // it will also append tasks from cache
		if err != nil {
			return err
		}

		if err := db.driver.Write(downloaderTasks, id, persistedTasks); err != nil {
			glog.Error(err)
			return err
		}

		db.taskInfoCache[id] = db.taskInfoCache[id][:0] // clear cache
	}
	return nil
}

func (db *downloaderDB) delete(id string) {
	db.mtx.Lock()
	db.driver.Delete(downloaderErrors, id)
	db.driver.Delete(downloaderTasks, id)
	db.mtx.Unlock()
}
