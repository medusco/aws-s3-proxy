package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type proxy struct {
	client       *s3.Client
	bucket       string
	cacheControl string
}

func main() {
	bucket := mustEnv("AWS_S3_BUCKET_NAME")
	endpoint := mustEnv("AWS_ENDPOINT_URL")
	region := envOr("AWS_DEFAULT_REGION", "auto")
	accessKey := mustEnv("AWS_ACCESS_KEY_ID")
	secretKey := mustEnv("AWS_SECRET_ACCESS_KEY")
	forcePathStyle := envOr("S3_FORCE_PATH_STYLE", "false") == "true"
	cacheControl := envOr("CACHE_CONTROL", "public, max-age=300")
	port := envOr("PORT", "8080")

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = forcePathStyle
	})

	p := &proxy{client: client, bucket: bucket, cacheControl: cacheControl}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", p.handle)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on :%s bucket=%s endpoint=%s", port, bucket, endpoint)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/")
	if key == "" || strings.HasSuffix(key, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:      aws.String(p.bucket),
		Key:         aws.String(key),
		IfNoneMatch: ifHeader(r, "If-None-Match"),
		IfMatch:     ifHeader(r, "If-Match"),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		var nf *types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Check if this is a 304 Not Modified response
		var ae smithy.APIError
		if errors.As(err, &ae) {
			if ae.ErrorCode() == "NotModified" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		log.Printf("get object %q: %v", key, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer out.Body.Close()

	h := w.Header()
	if out.ContentType != nil {
		h.Set("Content-Type", *out.ContentType)
	}
	if out.ContentLength != nil {
		h.Set("Content-Length", itoa(*out.ContentLength))
	}
	if out.ETag != nil {
		h.Set("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		h.Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	if p.cacheControl != "" {
		h.Set("Cache-Control", p.cacheControl)
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, out.Body); err != nil {
		log.Printf("stream %q: %v", key, err)
	}
}

func ifHeader(r *http.Request, name string) *string {
	v := r.Header.Get(name)
	if v == "" {
		return nil
	}
	return &v
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env var: %s", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
