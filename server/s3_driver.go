package server

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/s3/s3manager/s3manageriface"
	ftp "github.com/goftp/server"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func notEnabled(op string) error {
	return fmt.Errorf("%q is not enabled", op)
}

// S3Driver is a filesystem FTP driver.
// Implements https://godoc.org/github.com/goftp/server#Driver
type S3Driver struct {
	featureFlags int
	noOverwrite  bool
	s3           s3iface.S3API
	uploader     s3manageriface.UploaderAPI
	metrics      MetricsSender
	hostname     string
	bucketName   string
	bucketURL    *url.URL
	cwd          string
}

func intoAwsError(err error) awserr.Error {
	return err.(awserr.Error)
}

func logAwsError(err awserr.Error) {
	logrus.Errorf("AWS Error: Code=%q Message=%q", err.Code(), err.Message())
}

// bucketCheck checks if the bucket is accessible
func (d S3Driver) bucketCheck() error {
	_, err := d.s3.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(d.bucketName),
	})
	if err != nil {
		err := intoAwsError(err)
		logAwsError(err)
		logrus.Errorf("Bucket %q is not accessible.", d.bucketURL)
		return errors.Wrapf(err, "Bucket %q is not accessible", d.bucketName)
	}
	return nil
}

// Init initializes the FTP connection.
func (d S3Driver) Init(conn *ftp.Conn) {
	
}

// Stat returns information about the object with key `key`.
func (d S3Driver) Stat(key string) (ftp.FileInfo, error) {
	if err := d.bucketCheck(); err != nil {
		return S3ObjectInfo{}, errors.Wrapf(err, "Bucket check failed")
	}

	fqdn := d.fqdn(key)
	resp, err := d.s3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		err := intoAwsError(err)
		if err.Code() == "NotFound" {
			// If a client calls `ls` for a prefix (path) then `stat` is called for this prefix which will fail
			// in cases where the prefix is not an object key.
			// Returning an error would cause `ls` to fail, thus an ObjectInfo is returned which simulates a `stat` on a directory.
			return S3ObjectInfo{
				name:     key,
				isPrefix: true,
				size:     0,
				modTime:  time.Now(),
			}, nil
		}
		logrus.WithFields(logrus.Fields{"time": time.Now(), "object": fqdn}).Errorf("Stat for %q failed.\nCode: %s", fqdn, err.Code())
		return S3ObjectInfo{}, err
	}

	size := int64(0)
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	modTime := time.Now()
	if resp.LastModified != nil {
		modTime = *resp.LastModified
	}

	logrus.WithFields(logrus.Fields{"time": time.Now(), "key": fqdn, "action": "STAT"}).Infof("File information for %q", fqdn)
	return S3ObjectInfo{
		name:     key,
		isPrefix: true,
		size:     size,
		modTime:  modTime,
	}, nil
}

// ChangeDir will always return an error because there is no such operation for a cloud object storage.
//
// To allow uploading into "subdirectories" of a bucket a path change is simulated by keeping track of `CD` calls.
// In FTP only a single directory level will be changed at a time, i.e. `CD /foo/bar` will result in two calls, `CD /foo` and `CD /foo/bar`.
// There is no server side logic to be implement because relative paths are handled by the client, at least is how lftp and Filezilla operated.
func (d S3Driver) ChangeDir(path string) error {
	d.cwd = path
	logrus.Debugf("Changed into path: %q", d.cwd)
	return nil
}

// ListDir call the callback function with object metadata for each object located under prefix `key`.
func (d S3Driver) ListDir(key string, cb func(ftp.FileInfo) error) error {
	if d.featureFlags&featureList == 0 {
		return notEnabled("LS")
	}

	if err := d.bucketCheck(); err != nil {
		return errors.Wrapf(err, "Bucket check failed")
	}

	resp, err := d.s3.ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(d.bucketName),
	})
	if err != nil {
		err := intoAwsError(err)
		fqdn := d.fqdn(key)
		logAwsError(err)
		logrus.Errorf("Could not list %q.", fqdn)
		return err
	}
	prefixKey := strings.TrimPrefix(key, "/")
	folders := make(map[string]struct{})

	for _, object := range resp.Contents {
		name := *object.Key

		if prefixKey != "" {
			if !strings.HasPrefix(name, prefixKey) {
				continue
			} else {
				name = strings.TrimPrefix(name, prefixKey)
			}
		}

		owner := ""
		if object.Owner != nil {
			owner = *object.Owner.ID
		}

		isPrefix := false
		if trimName := strings.TrimPrefix(name, "/"); strings.Contains(trimName, "/") {
			isPrefix = true
			name = strings.Split(name, "/")[0]
		}

		finalName := strings.TrimPrefix(name, "/")
		//logrus.Infof("LS File: %q, Size: %d, IsPrefix: %v, Owner: %q, Last Modifier: %q", finalName, *object.Size,isPrefix, owner,*object.LastModified)

		if isPrefix {
			//check if already added
			if _, ok := folders[finalName]; ok {
				continue
			} else {
				folders[finalName] = struct{}{}
			}
		}

		err = cb(S3ObjectInfo{
			name:     finalName,
			size:     *object.Size,
			owner:    owner,
			modTime:  *object.LastModified,
			isPrefix: isPrefix,
		})

		if err != nil {
			logrus.WithFields(logrus.Fields{"time": time.Now(), "error": err}).Errorf("Could not list %q", d.fqdn(name))
		}

	}
	logrus.WithFields(logrus.Fields{"time": time.Now(), "key": key, "action": "LS"}).Infof("Directory listing for %q", key)
	return nil
}

// DeleteDir will always return an error because there is no such operation for a cloud object storage.
func (d S3Driver) DeleteDir(key string) error {
	// NOTE: Bucket removal will not be implemented
	logrus.Warn("RemoveDir (RMDIR) is not supported.")
	return notEnabled("RMDIR")
}

// DeleteFile will delete the object with key `key`.
func (d S3Driver) DeleteFile(key string) error {
	if d.featureFlags&featureRemove == 0 {
		logrus.Warn("Remove (RM) is not enabled.")
		return notEnabled("RM")
	}

	fqdn := d.fqdn(key)
	_, err := d.s3.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		err := intoAwsError(err)
		logAwsError(err)
		logrus.WithFields(logrus.Fields{"time": time.Now(), "code": err.Code(), "error": err.Message()}).Errorf("Failed to delete object %q.", fqdn)
		return err
	}

	logrus.WithFields(logrus.Fields{"time": time.Now(), "key": fqdn, "action": "DELETE"}).Infof("Deleted %q", fqdn)
	return nil
}

// Rename will always return an error because there is no such operation for a cloud object storage.
func (d S3Driver) Rename(oldKey string, newKey string) error {
	// TODO: there is no direct method for s3, must be copied and removed
	logrus.Warn("Rename (MV) is not supported.")
	return notEnabled("MV")
}

// MakeDir will always return an error because there is no such operation for a cloud object storage.
func (d S3Driver) MakeDir(key string) error {
	// There is no s3 equivalent
	logrus.Warn("MakeDir (MkDir) is not supported.")
	return notEnabled("MKDIR")
}

// GetFile returns the object with key `key`.
func (d S3Driver) GetFile(key string, offset int64) (int64, io.ReadCloser, error) {
	if d.featureFlags&featureGet == 0 {
		return -1, nil, notEnabled("GET")
	}

	fqdn := d.fqdn(key)
	timestamp := time.Now()
	resp, err := d.s3.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		err := intoAwsError(err)
		logAwsError(err)
		if err.Code() == "NotFound" {
			logrus.WithFields(logrus.Fields{"time": timestamp, "Object": fqdn}).Errorf("Failed to get object: %q", fqdn)
		}
		return 0, nil, err
	}
	size := *resp.ContentLength
	logrus.WithFields(logrus.Fields{"time": timestamp, "operation": "GET", "object": fqdn}).Infof("Serving object: %s", fqdn)

	err = d.metrics.SendGet(size, timestamp)
	if err != nil {
		logrus.Errorf("Sending GET metrics failed: %s", err)
	}

	return size, resp.Body, nil
}

// PutFile stores the object with key `key`.
// The method returns an error with no-overwrite was set and the object already exists or appendMode was specified.
func (d S3Driver) PutFile(key string, data io.Reader, appendMode bool) (int64, error) {
	if d.featureFlags&featurePut == 0 {
		return -1, notEnabled("PUT")
	}
	if data == nil || reflect.ValueOf(data).IsNil() {
		logrus.Warn("PutFile was called with a nil valued io.Reader")
		return -1, fmt.Errorf("PUT with empty data")
	}

	fqdn := d.fqdn(key)
	if appendMode {
		err := fmt.Errorf("can not append to object %q because the backend does not support appending", fqdn)
		logrus.Error(err)
		return -1, err
	}

	timestamp := time.Now()
	if d.noOverwrite && d.objectExists(key) {
		err := fmt.Errorf("object %q already exists and overwriting is forbidden", fqdn)
		logrus.WithFields(logrus.Fields{"time": timestamp, "key": fqdn, "error": err}).Error(err)
		return -1, err
	}

	_, err := d.uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
		Body:   data,
	})
	if err != nil {
		err := fmt.Errorf("Failed to put object %q because reading from source failed", fqdn)
		logrus.WithFields(logrus.Fields{"time": timestamp, "object": fqdn, "action": "PUT", "error": err}).Error(err)
		return -1, err
	}
	size, err := d.objectSize(key)
	if err != nil {
		logrus.WithFields(logrus.Fields{"time": timestamp, "key": fqdn, "action": "PUT", "error": err}).Errorf("Could not determine size of %q", fqdn)
		return size, err
	}
	logrus.WithFields(logrus.Fields{"time": timestamp, "key": fqdn, "action": "PUT"}).Infof("Put %q", fqdn)

	err = d.metrics.SendPut(size, timestamp)
	if err != nil {
		logrus.Errorf("Sending PUT metrics failed: %s", err)
	}

	return size, nil
}

// fqdn returns the fully qualified name for a object with key `key`.
func (d S3Driver) fqdn(key string) string {
	u := d.bucketURL
	u.Path = filepath.Join(d.cwd, key)
	return u.String()
}

// objectExists returns true if the object exists.
func (d S3Driver) objectExists(key string) bool {
	logrus.Debugf("Trying to check if object %q exists.", d.fqdn(key))
	_, err := d.s3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		err := intoAwsError(err)
		if err.Code() == "NotFound" {
			return false
		}
		logrus.Debugf("Failed to check object %q", d.fqdn(key))
		return false
	}
	return true
}

// objectSize returns the size of the object.
func (d S3Driver) objectSize(key string) (int64, error) {
	logrus.Debugf("Trying to get size of object %q.", d.fqdn(key))
	resp, err := d.s3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(d.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		logrus.Debugf("Failed to check size of object %q", d.fqdn(key))
		return -1, errors.Wrapf(err, "Failed to check size of object %q", d.fqdn(key))
	}
	return aws.Int64Value(resp.ContentLength), nil
}
