// Package minio 封装 MinIO 对象存储的业务侧接口。
//
// 设计要点：
//   - Store 接口与实现分离：实现用 minio-go SDK，接口便于测试替身注入
//   - 桶管理（EnsureBucket）内置：首次写入自动建桶，调用方无需关心
//   - ObjectKey 生成规则：rags/kb_{kbID}/doc_{docID}/{hash}.{ext}
//     与 doc.file_hash 配合做去重（同内容跳过上传）
//   - 错误统一 wrap + 脱敏（AccessKey 不进错误消息）
package minio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aegis/pkg/conf"
	mclient "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store 对象存储操作集（业务侧最小接口）
type Store interface {
	// Put 上传文件，返回 object_key（桶内路径）
	Put(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error
	// Get 下载文件，返回读取器
	Get(ctx context.Context, objectKey string) (io.ReadCloser, error)
	// GetBytes 便捷方法：小文件直接返回 []byte
	GetBytes(ctx context.Context, objectKey string) ([]byte, error)
	// Delete 删除对象（文档删除时级联清理）
	Delete(ctx context.Context, objectKey string) error
	// Close 释放连接（minio-go 用 http transport，无需显式关闭）
	Close() error
	// Bucket 当前桶名
	Bucket() string
}

// ErrNotFound 对象不存在
type ErrNotFound struct {
	ObjectKey string
}

func (e *ErrNotFound) Error() string { return fmt.Sprintf("minio: 对象不存在 %s", e.ObjectKey) }

// ObjectKeyOf 生成桶内路径
func ObjectKeyOf(kbID, docID int64, fileHash, ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	if ext == "" {
		ext = "bin"
	}
	return fmt.Sprintf("rags/kb_%d/doc_%d/%s.%s", kbID, docID, fileHash, ext)
}

// objectKeyOfURL 从 MinIO 完整 URL 解析 object_key
func ObjectKeyOfURL(url string) string {
	if idx := strings.Index(url, "://"); idx >= 0 {
		url = url[idx+3:]
	}
	if idx := strings.Index(url, "/"); idx >= 0 {
		url = url[idx+1:]
	}
	if idx := strings.Index(url, "/"); idx >= 0 {
		return url[idx+1:]
	}
	return ""
}

// store 实现
type store struct {
	client *mclient.Client
	bucket string
	cfg    conf.MinioConf
}

// New 构造对象存储客户端。
// Endpoint / AccessKey / SecretKey 为空返回 error（调用方据此回退内存直存）。
// Bucket 为空默认 "aegis"。
// 启动时自动 EnsureBucket：桶不存在则创建。
func New(ctx context.Context, cfg conf.MinioConf) (*store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("minio: endpoint 未配置")
	}
	if cfg.AccessKey == "" {
		return nil, errors.New("minio: access_key 未配置")
	}
	if cfg.SecretKey == "" {
		return nil, errors.New("minio: secret_key 未配置")
	}
	bucket := cfg.Bucket
	if bucket == "" {
		bucket = "aegis"
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	cli, err := mclient.New(cfg.Endpoint, &mclient.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio: 构造客户端失败: %w", err)
	}

	// EnsureBucket：不存在则建、已存在则跳过。
	// 用 MakeBucket 自身做连通性校验（首次 RPC 若地址/鉴权错会在此暴露）。
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cli.MakeBucket(pingCtx, bucket, mclient.MakeBucketOptions{}); err != nil {
		resp := mclient.ToErrorResponse(err)
		if resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			return nil, fmt.Errorf("minio: 连接 %s / 建桶 %s 失败: %w", cfg.Endpoint, bucket, err)
		}
	}

	return &store{client: cli, bucket: bucket, cfg: cfg}, nil
}

func (s *store) Bucket() string { return s.bucket }

func (s *store) Close() error { return nil }

func (s *store) Put(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, s.bucket, objectKey, reader, size, mclient.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("minio: 上传 %s 失败: %w", objectKey, err)
	}
	return nil
}

func (s *store) Get(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, objectKey, mclient.GetObjectOptions{})
	if err != nil {
		if mclient.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, &ErrNotFound{ObjectKey: objectKey}
		}
		return nil, fmt.Errorf("minio: 下载 %s 失败: %w", objectKey, err)
	}
	return obj, nil
}

func (s *store) GetBytes(ctx context.Context, objectKey string) ([]byte, error) {
	r, err := s.Get(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (s *store) Delete(ctx context.Context, objectKey string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, objectKey, mclient.RemoveObjectOptions{}); err != nil {
		// 删除不存在的对象不算失败
		if mclient.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil
		}
		return fmt.Errorf("minio: 删除 %s 失败: %w", objectKey, err)
	}
	return nil
}
