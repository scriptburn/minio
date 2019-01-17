# etcd V3 Quickstart Guide [![Slack](https://slack.minio.io/slack?type=svg)](https://slack.minio.io)
etcd is a distributed key value store that provides a reliable way to store data across a cluster of machines.

## Get started
### 1. Prerequisites
- Docker 18.03 or above, refer here for [installation](https://docs.docker.com/install/).

### 2. Start etcd
etcd uses [gcr.io/etcd-development/etcd](https://console.cloud.google.com/gcr/images/etcd-development/GLOBAL/etcd) as a primary container registry.

```
rm -rf /tmp/etcd-data.tmp && mkdir -p /tmp/etcd-data.tmp && \
  docker rmi gcr.io/etcd-development/etcd:v3.3.9 || true && \
  docker run \
  -p 2379:2379 \
  -p 2380:2380 \
  --mount type=bind,source=/tmp/etcd-data.tmp,destination=/etcd-data \
  --name etcd-gcr-v3.3.9 \
  gcr.io/etcd-development/etcd:v3.3.9 \
  /usr/local/bin/etcd \
  --name s1 \
  --data-dir /etcd-data \
  --listen-client-urls http://0.0.0.0:2379 \
  --advertise-client-urls http://0.0.0.0:2379 \
  --listen-peer-urls http://0.0.0.0:2380 \
  --initial-advertise-peer-urls http://0.0.0.0:2380 \
  --initial-cluster s1=http://0.0.0.0:2380 \
  --initial-cluster-token tkn \
  --initial-cluster-state new
```

You may also setup etcd with TLS following this documentation [here](https://coreos.com/etcd/docs/latest/op-guide/security.html)

### 3. Setup Minio with etcd
Minio server expects environment variable for etcd as `MINIO_ETCD_ENDPOINTS`, this environment variable takes many comma separated entries.
```
export MINIO_ETCD_ENDPOINTS=http://localhost:2379
minio server /data
```

NOTE: If `etcd` is configured with `Client-to-server authentication with HTTPS client certificates` then you need to use additional envs such as `MINIO_ETCD_CLIENT_CERT` pointing to path to `etcd-client.crt` and `MINIO_ETCD_CLIENT_CERT_KEY` path to `etcd-client.key` .

### 4. Test with Minio STS API
Assuming that you have configured Minio server to support STS API by following the doc [Minio STS Quickstart Guide](https://docs.minio.io/docs/minio-sts-quickstart-guide) and once you have obtained the JWT from WSO2 as mentioned in [WSO2 Quickstart Guide](https://github.com/scriptburn/minio/blob/master/docs/sts/wso2.md).
```
go run client-grants.go -cid PoEgXP6uVO45IsENRngDXj5Au5Ya -csec eKsw6z8CtOJVBtrOWvhRWL4TUCga

##### Credentials
{
	"accessKey": "IRBLVDGN5QGMDCMO1X8V",
	"secretKey": "KzS3UZKE7xqNdtRbKyfcWgxBS6P1G4kwZn4DXKuY",
	"expiration": "2018-08-21T15:49:38-07:00",
	"sessionToken": "eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhY2Nlc3NLZXkiOiJJUkJMVkRHTjVRR01EQ01PMVg4ViIsImF1ZCI6IlBvRWdYUDZ1Vk80NUlzRU5SbmdEWGo1QXU1WWEiLCJhenAiOiJQb0VnWFA2dVZPNDVJc0VOUm5nRFhqNUF1NVlhIiwiZXhwIjoxNTM0ODkxNzc4LCJpYXQiOjE1MzQ4ODgxNzgsImlzcyI6Imh0dHBzOi8vbG9jYWxob3N0Ojk0NDMvb2F1dGgyL3Rva2VuIiwianRpIjoiMTg0NDMyOWMtZDY1YS00OGEzLTgyMjgtOWRmNzNmZTgzZDU2In0.4rKsZ8VkZnIS_ALzfTJ9UbEKPFlQVvIyuHw6AWTJcDFDVgQA2ooQHmH9wUDnhXBi1M7o8yWJ47DXP-TLPhwCgQ"
}
```

These credentials can now be used to perform Minio API operations, these credentials automatically expire in 1hr. To understand more about credential expiry duration and client grants STS API read further [here](https://github.com/scriptburn/minio/blob/master/docs/sts/client-grants.md).

## Explore Further
- [Minio STS Quickstart Guide](https://docs.minio.io/docs/minio-sts-quickstart-guide)
- [The Minio documentation website](https://docs.minio.io)