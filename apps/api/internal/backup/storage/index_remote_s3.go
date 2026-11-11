package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

type recoveryS3HTTP struct{ client *http.Client }

func (c recoveryS3HTTP) Do(request *http.Request) (*http.Response, error) {
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	data, readErr := recoveryBody(response, maxRecoveryPageBody)
	if readErr != nil {
		return nil, readErr
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func recoveryS3(d Destination, c Credentials, client *http.Client) *s3.Client {
	return s3.New(s3.Options{BaseEndpoint: aws.String(d.S3.Endpoint), Region: d.S3.Region, UsePathStyle: d.S3.PathStyle, HTTPClient: recoveryS3HTTP{client}, RetryMaxAttempts: 1, Logger: logging.Nop{}, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: "LanQinRecoveryIndex"}, nil
	})})
}

func readS3RecoveryIndex(ctx context.Context, api *s3.Client, bucket, object string) ([]byte, error) {
	result, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(object)})
	if err != nil {
		return nil, errRecoveryRemote
	}
	defer result.Body.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, result.Body}, MaxRecoveryIndexBytes+1))
	if err != nil || len(data) > MaxRecoveryIndexBytes {
		return nil, errRecoveryRemote
	}
	return data, nil
}

func publishS3RecoveryIndex(ctx context.Context, d Destination, object string, document []byte, c Credentials, client *http.Client, guard func() error) ([]byte, error) {
	if !validRecoveryIndexObject(d, object) {
		return nil, errRecoveryRemote
	}
	api := recoveryS3(d, c, client)
	if err := guard(); err != nil {
		return nil, err
	}
	_, err := api.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(d.S3.Bucket), Key: aws.String(object), Body: bytes.NewReader(document), ContentLength: aws.Int64(int64(len(document))), ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	if err != nil {
		var status interface{ HTTPStatusCode() int }
		if !errors.As(err, &status) || status.HTTPStatusCode() != http.StatusPreconditionFailed {
			return nil, errRecoveryRemote
		}
	}
	return readS3RecoveryIndex(ctx, api, d.S3.Bucket, object)
}

func discoverS3RecoveryIndexes(ctx context.Context, d Destination, key []byte, c Credentials, client *http.Client) ([][]byte, error) {
	api := recoveryS3(d, c, client)
	prefix := ""
	if d.S3.Prefix != "" {
		prefix = d.S3.Prefix + "/"
	}
	var token *string
	seen := map[string]bool{}
	keys := []string{}
	for page := 0; page < maxRecoveryPages; page++ {
		result, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(d.S3.Bucket), Prefix: aws.String(prefix + "lanqin-"), MaxKeys: aws.Int32(maxRecoveryIndexes), ContinuationToken: token})
		if err != nil || len(result.Contents) > maxRecoveryIndexes {
			return nil, errRecoveryRemote
		}
		for _, item := range result.Contents {
			name := aws.ToString(item.Key)
			if !strings.HasPrefix(name, prefix+"lanqin-") {
				return nil, errRecoveryRemote
			}
			if !strings.HasSuffix(name, recoveryIndexSuffix) {
				continue
			}
			if !validRecoveryIndexObject(d, name) || aws.ToInt64(item.Size) < 1 || aws.ToInt64(item.Size) > MaxRecoveryIndexBytes {
				return nil, errRecoveryRemote
			}
			keys = append(keys, name)
			if len(keys) > maxRecoveryIndexes {
				return nil, errRecoveryRemote
			}
		}
		next := aws.ToString(result.NextContinuationToken)
		if !aws.ToBool(result.IsTruncated) {
			if next != "" {
				return nil, errRecoveryRemote
			}
			break
		}
		if page == maxRecoveryPages-1 || !validRecoveryPageToken(next) || seen[next] {
			return nil, errRecoveryRemote
		}
		seen[next] = true
		token = aws.String(next)
	}
	documents := make([][]byte, 0, len(keys))
	seenKeys := map[string]bool{}
	for _, name := range keys {
		if seenKeys[name] {
			return nil, errRecoveryRemote
		}
		seenKeys[name] = true
		document, err := readS3RecoveryIndex(ctx, api, d.S3.Bucket, name)
		if err != nil {
			return nil, err
		}
		entry, err := OpenRecoveryIndex(d, document, key)
		if err != nil || entry.ObjectKey+recoveryIndexSuffix != name {
			return nil, errRecoveryRemote
		}
		documents = append(documents, document)
	}
	return documents, nil
}
