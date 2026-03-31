# orca-demo-web-service-01
simple HTTP-based web service that returns some sample response and ORCA headers to indicate utilization so GCLBs can efficiently load balance across endpoints

### usage

```
# basic call
$ curl localhost:8080
{"message":"Hello, World!"}

# generate a 500 error via query param
$ curl localhost:8080 --url-query "error=true"
Triggered Error
```