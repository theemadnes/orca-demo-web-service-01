# orca-demo-web-service-01
simple HTTP-based web service that returns some sample response and ORCA headers to indicate utilization so GCLBs can efficiently load balance across endpoints

### usage

when running locally, of course

```
# basic call
$ curl localhost:8080
{"message":"Hello, World!"}

# generate a 500 error via query param
$ curl localhost:8080 --url-query "error=true"
Triggered Error

# when using verbose curl, you can see the ORCA headers
$ curl localhost:8080/ -v
* Host localhost:8080 was resolved.
* IPv6: ::1
* IPv4: 127.0.0.1
*   Trying [::1]:8080...
* Immediate connect fail for ::1: Cannot assign requested address
*   Trying 127.0.0.1:8080...
* Established connection to localhost (127.0.0.1 port 8080) from 127.0.0.1 port 55428 
* using HTTP/1.x
> GET / HTTP/1.1
> Host: localhost:8080
> User-Agent: curl/8.18.0
> Accept: */*
> 
* Request completely sent off
< HTTP/1.1 200 OK
< Content-Type: application/json
< Endpoint-Load-Metrics-Json: JSON {"cpu_utilization":0.9107142857141945,"mem_utilization":0.10881329302224386,"application_utilization":0.1,"rps_fractional":39567.90880970756,"eps":0.19987729305119473,"named_metrics":{"total_errors":9,"total_requests":723880}}
< Date: Tue, 31 Mar 2026 04:47:43 GMT
< Content-Length: 28
< 
{"message":"Hello, World!"}
* Connection #0 to host localhost:8080 left intact
```