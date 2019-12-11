package s3ext

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/private/protocol/rest"
)

var (
	errInvalidMethod = errors.New("v2 signer does not handle HTTP POST")
)

const (
	signatureVersion = "2"
	signatureMethod  = "HmacSHA1"
	timeFormat       = "Mon, 2 Jan 2006 15:04:05 +0000"
)

var subresources = []string{
	"acl",
	"delete",
	"lifecycle",
	"location",
	"logging",
	"notification",
	"partNumber",
	"policy",
	"requestPayment",
	"torrent",
	"uploadId",
	"uploads",
	"versionId",
	"versioning",
	"versions",
	"website",
}

type signer struct {
	// Values that must be populated from the request
	Request     *http.Request
	Time        time.Time
	Credentials *credentials.Credentials
	Debug       aws.LogLevelType
	Logger      aws.Logger
	pathStyle   bool
	bucket      string

	Query        url.Values
	stringToSign string
	signature    string
}

// Sign requests with signature version 2.
//
// Will sign the requests with the service config's Credentials object
// Signing is skipped if the credentials is the credentials.AnonymousCredentials
// object.
func SignV2(req *request.Request) {
	// If the request does not need to be signed ignore the signing of the
	// request if the AnonymousCredentials object is used.
	if req.Config.Credentials == credentials.AnonymousCredentials {
		return
	}

	v2 := signer{
		Request:     req.HTTPRequest,
		Time:        req.Time,
		Credentials: req.Config.Credentials,
		Debug:       req.Config.LogLevel.Value(),
		Logger:      req.Config.Logger,
		pathStyle:   aws.BoolValue(req.Config.S3ForcePathStyle),
	}

	req.Error = v2.Sign()
}

func (v2 *signer) Sign() error {
	credValue, err := v2.Credentials.Get()
	if err != nil {
		return err
	}

	v2.Query = v2.Request.URL.Query()

	contentMD5 := v2.Request.Header.Get("Content-MD5")
	contentType := v2.Request.Header.Get("Content-Type")
	date := v2.Time.UTC().Format(timeFormat)
	v2.Request.Header.Set("x-amz-date", date)

	if credValue.SessionToken != "" {
		v2.Request.Header.Set("x-amz-security-token", credValue.SessionToken)
	}

	// in case this is a retry, ensure no signature present
	v2.Request.Header.Del("Authorization")

	method := v2.Request.Method

	uri := v2.Request.URL.Opaque
	if uri != "" {
		if questionMark := strings.Index(uri, "?"); questionMark != -1 {
			uri = uri[0:questionMark]
		}
		uri = "/" + strings.Join(strings.Split(uri, "/")[3:], "/")
	} else {
		uri = v2.Request.URL.Path
	}
	path := rest.EscapePath(uri, false)
	if !v2.pathStyle {
		host := strings.SplitN(v2.Request.URL.Host, ".", 2)[0]
		path = "/" + host + uri
	}
	if path == "" {
		path = "/"
	}

	// build URL-encoded query keys and values
	queryKeysAndValues := []string{}
	for _, key := range subresources {
		if _, ok := v2.Query[key]; ok {
			k := strings.Replace(url.QueryEscape(key), "+", "%20", -1)
			v := strings.Replace(url.QueryEscape(v2.Query.Get(key)), "+", "%20", -1)
			if v != "" {
				v = "=" + v
			}
			queryKeysAndValues = append(queryKeysAndValues, k+v)
		}
	}

	// join into one query string
	query := strings.Join(queryKeysAndValues, "&")

	if query != "" {
		path += "?" + query
	}

	tmp := []string{
		method,
		contentMD5,
		contentType,
		"",
	}

	var headers []string
	for k := range v2.Request.Header {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, "x-amz-") {
			headers = append(headers, k)
		}
	}
	sort.Strings(headers)

	for _, k := range headers {
		v := strings.Join(v2.Request.Header[http.CanonicalHeaderKey(k)], ",")
		tmp = append(tmp, k+":"+v)
	}

	tmp = append(tmp, path)

	// build the canonical string for the V2 signature
	v2.stringToSign = strings.Join(tmp, "\n")

	hash := hmac.New(sha1.New, []byte(credValue.SecretAccessKey))
	hash.Write([]byte(v2.stringToSign))
	v2.signature = base64.StdEncoding.EncodeToString(hash.Sum(nil))
	v2.Request.Header.Set("Authorization",
		"AWS "+credValue.AccessKeyID+":"+v2.signature)

	if v2.Debug.Matches(aws.LogDebugWithSigning) {
		v2.logSigningInfo()
	}

	return nil
}

const logSignInfoMsg = `DEBUG: Request Signature:
---[ STRING TO SIGN ]--------------------------------
%s
---[ SIGNATURE ]-------------------------------------
%s
-----------------------------------------------------`

func (v2 *signer) logSigningInfo() {
	msg := fmt.Sprintf(logSignInfoMsg, v2.stringToSign, v2.Request.Header.Get("Authorization"))
	v2.Logger.Log(msg)
}
