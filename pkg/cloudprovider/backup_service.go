/*
Copyright 2017 the Heptio Ark Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudprovider

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"

	api "github.com/heptio/ark/pkg/apis/ark/v1"
	"github.com/heptio/ark/pkg/generated/clientset/versioned/scheme"
)

// BackupService contains methods for working with backups in object storage.
type BackupService interface {
	BackupGetter
	// UploadBackup uploads the specified Ark backup of a set of Kubernetes API objects, whose manifests are
	// stored in the specified file, into object storage in an Ark bucket, tagged with Ark metadata. Returns
	// an error if a problem is encountered accessing the file or performing the upload via the cloud API.
	UploadBackup(bucket, name string, metadata, backup, log io.Reader) error

	// DownloadBackup downloads an Ark backup with the specified object key from object storage via the cloud API.
	// It returns the snapshot metadata and data (separately), or an error if a problem is encountered
	// downloading or reading the file from the cloud API.
	DownloadBackup(bucket, name string) (io.ReadCloser, error)

	// DeleteBackupDir deletes all files in object storage for the given backup.
	DeleteBackupDir(bucket, backupName string) error

	// GetBackup gets the specified api.Backup from the given bucket in object storage.
	GetBackup(bucket, name string) (*api.Backup, error)

	// CreateSignedURL creates a pre-signed URL that can be used to download a file from object
	// storage. The URL expires after ttl.
	CreateSignedURL(target api.DownloadTarget, bucket string, ttl time.Duration) (string, error)

	// UploadRestoreLog uploads the restore's log file to object storage.
	UploadRestoreLog(bucket, backup, restore string, log io.Reader) error

	// UploadRestoreResults uploads the restore's results file to object storage.
	UploadRestoreResults(bucket, backup, restore string, results io.Reader) error
}

// BackupGetter knows how to list backups in object storage.
type BackupGetter interface {
	// GetAllBackups lists all the api.Backups in object storage for the given bucket.
	GetAllBackups(bucket string) ([]*api.Backup, error)
}

const (
	metadataFileFormatString       = "%s/ark-backup.json"
	backupFileFormatString         = "%s/%s.tar.gz"
	backupLogFileFormatString      = "%s/%s-logs.gz"
	restoreLogFileFormatString     = "%s/restore-%s-logs.gz"
	restoreResultsFileFormatString = "%s/restore-%s-results.gz"
)

func getMetadataKey(backup string) string {
	return fmt.Sprintf(metadataFileFormatString, backup)
}

func getBackupContentsKey(backup string) string {
	return fmt.Sprintf(backupFileFormatString, backup, backup)
}

func getBackupLogKey(backup string) string {
	return fmt.Sprintf(backupLogFileFormatString, backup, backup)
}

func getRestoreLogKey(backup, restore string) string {
	return fmt.Sprintf(restoreLogFileFormatString, backup, restore)
}

func getRestoreResultsKey(backup, restore string) string {
	return fmt.Sprintf(restoreResultsFileFormatString, backup, restore)
}

type backupService struct {
	objectStore ObjectStore
	decoder     runtime.Decoder
	logger      logrus.FieldLogger
}

var _ BackupService = &backupService{}
var _ BackupGetter = &backupService{}

// NewBackupService creates a backup service using the provided object store
func NewBackupService(objectStore ObjectStore, logger logrus.FieldLogger) BackupService {
	return &backupService{
		objectStore: objectStore,
		decoder:     scheme.Codecs.UniversalDecoder(api.SchemeGroupVersion),
		logger:      logger,
	}
}

func (br *backupService) UploadBackup(bucket, backupName string, metadata, backup, log io.Reader) error {
	// upload metadata file
	metadataKey := getMetadataKey(backupName)
	if err := br.objectStore.PutObject(bucket, metadataKey, metadata); err != nil {
		// failure to upload metadata file is a hard-stop
		return err
	}

	// upload tar file
	if err := br.objectStore.PutObject(bucket, getBackupContentsKey(backupName), backup); err != nil {
		// try to delete the metadata file since the data upload failed
		deleteErr := br.objectStore.DeleteObject(bucket, metadataKey)

		return kerrors.NewAggregate([]error{err, deleteErr})
	}

	// uploading log file is best-effort; if it fails, we log the error but call the overall upload a
	// success
	logKey := getBackupLogKey(backupName)
	if err := br.objectStore.PutObject(bucket, logKey, log); err != nil {
		br.logger.WithError(err).WithFields(logrus.Fields{
			"bucket": bucket,
			"key":    logKey,
		}).Error("Error uploading log file")
	}

	return nil
}

func (br *backupService) DownloadBackup(bucket, backupName string) (io.ReadCloser, error) {
	return br.objectStore.GetObject(bucket, getBackupContentsKey(backupName))
}

func (br *backupService) GetAllBackups(bucket string) ([]*api.Backup, error) {
	prefixes, err := br.objectStore.ListCommonPrefixes(bucket, "/")
	if err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return []*api.Backup{}, nil
	}

	output := make([]*api.Backup, 0, len(prefixes))

	for _, backupDir := range prefixes {
		backup, err := br.GetBackup(bucket, backupDir)
		if err != nil {
			br.logger.WithError(err).WithField("dir", backupDir).Error("Error reading backup directory")
			continue
		}

		output = append(output, backup)
	}

	return output, nil
}

func (br *backupService) GetBackup(bucket, name string) (*api.Backup, error) {
	key := fmt.Sprintf(metadataFileFormatString, name)

	res, err := br.objectStore.GetObject(bucket, key)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	data, err := ioutil.ReadAll(res)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	obj, _, err := br.decoder.Decode(data, nil, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	backup, ok := obj.(*api.Backup)
	if !ok {
		return nil, errors.Errorf("unexpected type for %s/%s: %T", bucket, key, obj)
	}

	return backup, nil
}

func (br *backupService) DeleteBackupDir(bucket, backupName string) error {
	objects, err := br.objectStore.ListObjects(bucket, backupName+"/")
	if err != nil {
		return err
	}

	var errs []error
	for _, key := range objects {
		br.logger.WithFields(logrus.Fields{
			"bucket": bucket,
			"key":    key,
		}).Debug("Trying to delete object")
		if err := br.objectStore.DeleteObject(bucket, key); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.WithStack(kerrors.NewAggregate(errs))
}

func (br *backupService) CreateSignedURL(target api.DownloadTarget, bucket string, ttl time.Duration) (string, error) {
	switch target.Kind {
	case api.DownloadTargetKindBackupContents:
		return br.objectStore.CreateSignedURL(bucket, getBackupContentsKey(target.Name), ttl)
	case api.DownloadTargetKindBackupLog:
		return br.objectStore.CreateSignedURL(bucket, getBackupLogKey(target.Name), ttl)
	case api.DownloadTargetKindRestoreLog:
		backup := extractBackupName(target.Name)
		return br.objectStore.CreateSignedURL(bucket, getRestoreLogKey(backup, target.Name), ttl)
	case api.DownloadTargetKindRestoreResults:
		backup := extractBackupName(target.Name)
		return br.objectStore.CreateSignedURL(bucket, getRestoreResultsKey(backup, target.Name), ttl)
	default:
		return "", errors.Errorf("unsupported download target kind %q", target.Kind)
	}
}

func extractBackupName(s string) string {
	// restore name is formatted as <backup name>-<timestamp>
	i := strings.LastIndex(s, "-")
	if i < 0 {
		i = len(s)
	}
	return s[0:i]
}

func (br *backupService) UploadRestoreLog(bucket, backup, restore string, log io.Reader) error {
	key := getRestoreLogKey(backup, restore)
	return br.objectStore.PutObject(bucket, key, log)
}

func (br *backupService) UploadRestoreResults(bucket, backup, restore string, results io.Reader) error {
	key := getRestoreResultsKey(backup, restore)
	return br.objectStore.PutObject(bucket, key, results)
}

// cachedBackupService wraps a real backup service with a cache for getting cloud backups.
type cachedBackupService struct {
	BackupService
	cache BackupGetter
}

// NewBackupServiceWithCachedBackupGetter returns a BackupService that uses a cache for
// GetAllBackups().
func NewBackupServiceWithCachedBackupGetter(
	ctx context.Context,
	delegate BackupService,
	resyncPeriod time.Duration,
	logger logrus.FieldLogger,
) BackupService {
	return &cachedBackupService{
		BackupService: delegate,
		cache:         NewBackupCache(ctx, delegate, resyncPeriod, logger),
	}
}

func (c *cachedBackupService) GetAllBackups(bucketName string) ([]*api.Backup, error) {
	return c.cache.GetAllBackups(bucketName)
}
