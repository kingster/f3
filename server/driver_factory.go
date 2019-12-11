package server

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws/request"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/spreadshirt/f3/s3ext"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	ftp "github.com/goftp/server"
	goErrors "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// DefaultFeatureSet is the driver default (set of) features
	DefaultFeatureSet = "ls"
	// DefaultRegion is the default bucket region
	DefaultRegion = "custom"
)

// DriverFactory builds FTP drivers.
// Implements https://godoc.org/github.com/goftp/server#DriverFactory
type DriverFactory struct {
	featureFlags      int
	noOverwrite       bool
	awsCredentials    *credentials.Credentials
	s3PathStyle       bool
	s3SignatureV2     bool
	s3Region          string
	s3Endpoint        string
	hostname          string
	bucketName        string
	bucketURL         *url.URL
	DisableCloudWatch bool
	DisableSSL        bool
}

// NewDriver returns a new FTP driver.
func (d DriverFactory) NewDriver() (ftp.Driver, error) {
	logrus.Debugf("Trying to create an aws session with: Region: %q, PathStyle: %v, Endpoint: %q", d.s3Region, d.s3PathStyle, d.s3Endpoint)
	s3Session, err := session.NewSession(&aws.Config{
		Region:           aws.String(d.s3Region),
		S3ForcePathStyle: aws.Bool(d.s3PathStyle),
		Endpoint:         aws.String(d.s3Endpoint),
		Credentials:      d.awsCredentials,
		DisableSSL:       aws.Bool(d.DisableSSL),
	})
	if err != nil {
		return nil, goErrors.Wrapf(err, "Failed to instantiate driver")
	}
	s3Client := s3.New(s3Session)

	if d.s3SignatureV2 {
		logrus.Debug("Using Signature V2 Format")
		s3Client.Handlers.Sign.Swap(v4.SignRequestHandler.Name, request.NamedHandler{
			Name: "v2Signer",
			Fn: func(req *request.Request) {
				s3ext.SignV2(req)
			},
		})
	}

	var metricsSender MetricsSender
	if d.DisableCloudWatch {
		metricsSender = NopSender{}
	} else {
		cloudwatchSession, err := session.NewSession(&aws.Config{
			Region:      aws.String(d.s3Region),
			Credentials: d.awsCredentials,
		})
		if err != nil {
			return nil, goErrors.Wrapf(err, "Failed to create cloudwatch session")
		}

		metricsSender, err = NewCloudwatchSender(cloudwatchSession)
		if err != nil {
			return nil, goErrors.Wrapf(err, "Failed to instantiate cloudwatch sender")
		}
	}
	return S3Driver{
		featureFlags: d.featureFlags,
		noOverwrite:  d.noOverwrite,
		s3:           s3Client,
		uploader:     s3manager.NewUploaderWithClient(s3Client),
		metrics:      metricsSender,
		bucketName:   d.bucketName,
		bucketURL:    d.bucketURL,
	}, nil
}

// FactoryConfig wraps config values required to setup an FTP driver and for the s3 backend.
type FactoryConfig struct {
	FtpFeatures       string
	FtpNoOverwrite    bool
	S3Credentials     string
	S3BucketURL       string
	S3Region          string
	S3Endpoint        string
	S3UsePathStyle    bool
	S3SignatureV2     bool
	DisableCloudWatch bool
	S3DisableSSL      bool
}

// NewDriverFactory returns a DriverFactory.
func NewDriverFactory(config *FactoryConfig) (DriverFactory, error) {
	_, factory, err := setupS3(setupFtp(config, &DriverFactory{}, nil))
	factory.DisableCloudWatch = config.DisableCloudWatch
	return *factory, err
}

func setupFtp(config *FactoryConfig, factory *DriverFactory, err error) (*FactoryConfig, *DriverFactory, error) {
	if err != nil { // fallthrough
		return config, factory, err
	}
	factory.noOverwrite = config.FtpNoOverwrite

	logrus.Debugf("Trying to parse feature set: %q", config.FtpFeatures)
	featureFlags, err := parseFeatureSet(config.FtpFeatures)
	if err != nil {
		return config, factory, goErrors.Wrapf(err, "Failed to parse FTP feature set: %q", config.FtpFeatures)
	}
	factory.featureFlags = featureFlags

	return config, factory, nil
}

const (
	featureChangeDir = 1 << iota
	featureList      = 1 << iota
	featureRemoveDir = 1 << iota
	featureRemove    = 1 << iota
	featureMove      = 1 << iota
	featureMakeDir   = 1 << iota
	featureGet       = 1 << iota
	featurePut       = 1 << iota
)

func parseFeatureSet(featureSet string) (int, error) {
	featureFlags := 0
	featureSet = strings.TrimSpace(featureSet)
	if featureSet == "" {
		return featureFlags, errors.New("Empty feature set")
	}
	features := strings.Split(featureSet, ",")
	for _, feature := range features {
		switch strings.ToLower(feature) {
		case "cd":
			featureFlags |= featureChangeDir
		case "ls":
			featureFlags |= featureList
		case "rmdir":
			featureFlags |= featureRemoveDir
		case "rm":
			featureFlags |= featureRemove
		case "mv":
			featureFlags |= featureMove
		case "mkdir":
			featureFlags |= featureMakeDir
		case "get":
			featureFlags |= featureGet
		case "put":
			featureFlags |= featurePut
		default:
			return 0, fmt.Errorf("Unknown feature flag: %q", feature)
		}
	}
	return featureFlags, nil
}

func setupS3(config *FactoryConfig, factory *DriverFactory, err error) (*FactoryConfig, *DriverFactory, error) {
	if err != nil { // fallthrough
		return config, factory, err
	}

	// credentials
	pair := strings.SplitN(config.S3Credentials, ":", 2)
	if len(pair) != 2 {
		return config, factory, fmt.Errorf("Malformed credentials, not in format: 'access_key:secret_key'")
	}
	accessKey, secretKey := pair[0], pair[1]
	sessionToken := ""
	factory.awsCredentials = credentials.NewStaticCredentials(accessKey, secretKey, sessionToken)

	bucketURL, err := url.Parse(config.S3BucketURL)
	if err != nil {
		return config, factory, goErrors.Wrapf(err, "Failed to parse s3 bucket URL: %q", config.S3BucketURL)
	}
	factory.bucketURL = bucketURL

	if config.S3Endpoint == "" {
		// retrieve bucket name and endpoint from bucket FQDN
		pair = strings.SplitN(bucketURL.Host, ".", 2)
		if len(pair) != 2 {
			return config, factory, fmt.Errorf("Not a fully qualified bucket name (e.g. 'bucket.host.domain'): %q", bucketURL.String())
		}
		bucketName, endpoint := pair[0], fmt.Sprintf("%s://%s", bucketURL.Scheme, pair[1])
		factory.bucketName = bucketName
		factory.s3Endpoint = endpoint
	} else {
		factory.bucketName = bucketURL.Host
		factory.s3Endpoint = config.S3Endpoint
	}

	factory.s3Region = config.S3Region
	factory.s3PathStyle = config.S3UsePathStyle
	factory.s3SignatureV2 = config.S3SignatureV2
	factory.DisableSSL = config.S3DisableSSL

	return config, factory, nil
}
