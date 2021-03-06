package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akamensky/argparse"
	_ "github.com/lib/pq"
	"github.com/pierrec/lz4"
	"github.com/thumbtack/pgCarpenter/util"
	"go.uber.org/zap"
)

// there's no point on taking backups of directories like log or pg_xlog
var prefixesNotToBackup = []string{"log", "pg_xlog", "postmaster.pid", "pg_replslot"}

func (a *app) createBackup() int {
	a.logger.Info("Preparing to start backup", zap.String("name", *a.backupName))
	begin := time.Now()

	backupKey := *a.backupName + "/"

	// don't allow existing backups to be overwritten
	_, err := a.storage.GetString(backupKey)
	if err == nil {
		a.logger.Error("A backup with the same name already exists", zap.String("backup_name", *a.backupName))
		return 1
	}

	// create the top level "folder" so that the object actually exists and
	// has all the relevant metadata like timestamps
	if err := a.storage.PutString(backupKey, ""); err != nil {
		a.logger.Error("Failed to create top-level backup folder", zap.Error(err))
		return 1
	}

	// tell PG we're starting a base backup, copy all the file, tell PG we're done
	db, err := a.startBackup()
	if err != nil {
		a.logger.Error("Failed to start backup", zap.Error(err))
		return 1
	}

	// copy all files to remote storage
	items := a.uploadFiles()

	// tell PG we're done copying the data directory, save the tablespace map and backup label files
	if err := a.stopBackup(db); err != nil {
		a.logger.Error("Failed to stop backup", zap.Error(err))
		return 1
	}

	// mark the backup as successful
	if err := a.putSuccessfulMarker(*a.backupName); err != nil {
		a.logger.Error("Failed to mark backup as successfully completed", zap.Error(err))
	}

	// update the LATEST marker
	if err := a.updateLatest(*a.backupName); err != nil {
		a.logger.Error("Failed to update the LATEST marker", zap.Error(err))
		return 1
	}

	a.logger.Info(
		"Backup successfully completed",
		zap.String("name", *a.backupName),
		zap.Int("files", items),
		zap.Duration("seconds", time.Now().Sub(begin)),
	)

	return 0
}

func (a *app) startBackup() (*sql.Conn, error) {
	a.logger.Info("Starting backup", zap.String("name", *a.backupName))
	d := time.Now().Add(time.Duration(*a.statementTimeout) * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), d)
	defer cancel()

	connStr := fmt.Sprintf("user=%s password='%s' sslmode=%s", *a.pgUser, *a.pgPassword, *a.sslMode)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}

	_, err = conn.QueryContext(
		ctx,
		"SELECT pg_start_backup($1, $2, $3)",
		*a.backupName,
		*a.backupCheckpoint,
		"false",
	)
	if err != nil {
		return nil, err
	}

	// when doing a non-exclusive backup connection calling pg_start_backup must be maintained until the end of the
	// backup, or the backup will be automatically aborted
	return conn, nil
}

func (a *app) stopBackup(conn *sql.Conn) error {
	a.logger.Info("Stopping backup", zap.String("name", *a.backupName))
	var lsn, labelFile, mapFile string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// print a short message to indicate we're just waiting for pg_stop_backup to complete
	//
	// pg_stop_backup will only succeed after all the necessary WAL has been
	// archived, which may take a while
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(60 * time.Second)
				a.logger.Info("Waiting for pg_stop_backup")
			}
		}

	}()

	row := conn.QueryRowContext(ctx, "SELECT * FROM pg_stop_backup(false)")
	err := row.Scan(&lsn, &labelFile, &mapFile)
	if err != nil {
		return err
	}

	// explicitly close the connection we kept open throughout the backup
	err = conn.Close()
	if err != nil {
		a.logger.Error("Failed to close connection", zap.Error(err))
	}

	// upload the second field to a file named backup_label in the root directory of the backup and
	// the third field to a file named tablespace_map, unless the field is empty
	key := *a.backupName + "/backup_label"
	err = a.storage.PutString(key, labelFile)
	if err != nil {
		return err
	}

	if mapFile != "" {
		key = *a.backupName + "/tablespace_map"
		err = a.storage.PutString(key, mapFile)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *app) getSuccessfulMarker(backupName string) string {
	return filepath.Join(successfullyCompletedFolder, backupName)
}

func (a *app) putSuccessfulMarker(backupName string) error {
	return a.storage.PutString(a.getSuccessfulMarker(backupName), "")
}

func (a *app) deleteSuccessfulMarker(backupName string) error {
	key := a.getSuccessfulMarker(backupName)
	_, err := a.storage.GetString(key)
	if err == nil {
		if err := a.storage.Delete(key); err != nil {
			return err
		}
	}

	return nil
}

func (a *app) updateLatest(backupName string) error {
	return a.storage.PutString(latestKey, backupName)
}

// upload the data directory to remote storage; return the number of files uploaded
func (a *app) uploadFiles() int {
	a.logger.Info("Preparing to upload files", zap.String("name", *a.backupName))
	// channel to keep the path of all files that need to compressed and uploaded
	filesC := make(chan string)

	// spawn a pool of workers
	a.logger.Info("Spawning workers", zap.Int("number", *a.nWorkers))
	wg := &sync.WaitGroup{}
	wg.Add(*a.nWorkers)
	for i := 0; i < *a.nWorkers; i++ {
		go a.backupWorker(filesC, wg)
	}

	// traverse the data directory and put each file (relative path) in the channel for a worker to process
	a.logger.Info("Traversing the data directory", zap.String("path", *a.pgDataDirectory))
	items := 0
	err := filepath.Walk(
		*a.pgDataDirectory,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// files might change during the copy process; it's normal during an online backup
				if os.IsNotExist(err) {
					a.logger.Debug("Source file vanished", zap.String("path", path), zap.Error(err))
					return nil
				}
				// anything other than the file not existing, on the other hand, is a problem
				return err
			}
			// grab just the path relative to the data directory
			file := strings.TrimPrefix(path, *a.pgDataDirectory)
			if a.ignoreFile(file) {
				a.logger.Debug("Ignoring file", zap.String("path", path))
				return nil
			}
			a.logger.Debug("Adding file", zap.String("path", file))
			filesC <- file
			items++
			return nil
		},
	)

	if err != nil {
		a.logger.Error("Failed to walk data directory", zap.Error(err))
		return 1
	}

	a.logger.Info("Waiting for all workers to finish")
	close(filesC)
	wg.Wait()

	return items
}

// return true iff it's in one of the directories we do not need to backup
func (a *app) ignoreFile(path string) bool {
	for _, d := range prefixesNotToBackup {
		if strings.HasPrefix(path, d) {
			return true
		}
	}

	return false
}

// continuously receive file paths (relative to the data directory) from the filesC channel
// compress the ones larger than compress-threshold, and upload them to remote storage along with some relevant metadata
func (a *app) backupWorker(filesC <-chan string, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		pgFile, more := <-filesC
		if !more {
			a.logger.Debug("No more files to process")
			return
		}

		pgFilePath := filepath.Join(*a.pgDataDirectory, pgFile)
		st, err := os.Stat(pgFilePath)
		if err != nil {
			// this can happen for very legitimate reasons, as PG is not stopped and we're taking an online backup
			a.logger.Info("Failed to stat file. Might have been removed", zap.Error(err))
			continue
		}

		// name the object after the file path relative to the data directory
		key := filepath.Join(*a.backupName, pgFile)
		// create directories
		// some directories (e.g., pg_logical/mappings) need to exist even if empty otherwise
		// PG, while fully functional, will continuously log an error message
		if st.IsDir() {
			// append the extension that identifies this object as a directory
			key += util.DirectoryExtension
			a.logger.Debug(
				"Creating object for directory directory",
				zap.String("path", pgFile),
				zap.String("key", key))
			if err := a.storage.PutString(key, ""); err != nil {
				a.logger.Fatal("Failed to create object for directory on remote storage", zap.Error(err))
			}
			continue
		}
		// compress files larger than a given threshold
		compressed := ""
		if st.Size() > int64(*a.compressThreshold) {
			a.logger.Debug("Compressing file", zap.String("path", pgFile), zap.Int64("size", st.Size()))
			compressed, err = util.Compress(pgFilePath, *a.tmpDirectory)
			if err != nil {
				a.logger.Error("Failed to compress file", zap.Error(err))
				// we use compressed == "" to decide whether to upload and remove a compressed file
				// let's try to proceed with the backup by uploading the uncompressed file
				compressed = ""
				continue
			}
			// mark the object as a compressed file
			key += lz4.Extension
		}

		if compressed != "" {
			err = a.storage.Put(key, compressed, st.ModTime().Unix())
			// cleanup the temporary compressed file
			util.MustRemoveFile(compressed, a.logger)
		} else {
			err = a.storage.Put(key, pgFilePath, st.ModTime().Unix())
		}

		if err != nil {
			a.logger.Fatal("Failed to upload file", zap.Error(err))
		}
	}
}

func parseCreateBackupArgs(cfg *app, parser *argparse.Command) {
	cfg.compressThreshold = parser.Int(
		"",
		"compress-threshold",
		&argparse.Options{
			Required: false,
			Default:  512 * 1024,
			Help:     "compress files larger than"})
	cfg.pgUser = parser.String(
		"",
		"user",
		&argparse.Options{
			Required: false,
			Default:  "postgres",
			Help:     "PostgreSQL user"})
	cfg.pgPassword = parser.String(
		"",
		"password",
		&argparse.Options{
			Required: false,
			Default:  "",
			Help:     "PostgreSQL password"})
	cfg.backupCheckpoint = parser.Flag(
		"",
		"checkpoint",
		&argparse.Options{
			Required: false,
			Default:  false,
			Help:     "Start the backup as soon as possible by issuing an checkpoint"})
	cfg.sslMode = parser.Selector(
		"",
		"sslmode",
		[]string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"},
		&argparse.Options{
			Required: false,
			Default:  "disable",
			Help:     "SSL certificate verification mode"})
	cfg.statementTimeout = parser.Int(
		"",
		"statement-timeout",
		&argparse.Options{
			Required: false,
			Default:  60,
			Help:     "Cancel a start/stop backup statement if it takes more than the specified number of seconds"})
}
