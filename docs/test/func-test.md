# 接受SRT推流
obs64.exe --portable --profile tx-test-01 --collection tx-test-01
srt://localhost:8890?streamid=publish:live/sabado-fwv-hd4&latency=200000&pkt_size=1316
play:
webrtc: http://127.0.0.1:8889/live/sabado-fwv-hd4
http://127.0.0.1:8900/live/sabado-fwv-hd4

SRT推流/拉流，只测origin node，时延6s。
obs publish srt://localhost:8890?streamid=publish:live/sabado3
ffplay "srt://127.0.0.1:8890?streamid=read:live/sabado3"
obs推流设置gop为1s，时延一样很大！
这就是协议的原因，比RTMP推流，FLV拉流的时延还要大！

改为WHIP推流：http://localhost:8889/live/test/whip
WHEP拉流: http://localhost:8889/live/test
时延就缩小到1s以内。



===================================
测试: obs推SRT到origin节点，origin转发edge节点，从edge拉流webrtc。
运行日志如下：
D:\download\p2pcdnall\mmx.git\bin>mmx.exe mediamtx_orgi.yml
[90m2025/12/29 15:17:11 [0m[32mINF[0m MediaMTX v1.15.5

[90m2025/12/29 15:17:11 [0m[32mINF[0m configuration loaded from D:\download\p2pcdnall\mmx.git\bin\mediamtx_orgi.yml
[90m2025/12/29 15:17:11 [0m[32mINF[0m [RTSP] listener opened on :8554 (TCP), :8000 (UDP/RTP), :8001 (UDP/RTCP)
[90m2025/12/29 15:17:11 [0m[32mINF[0m [RTMP] listener opened on :1935
[90m2025/12/29 15:17:11 [0m[32mINF[0m [WebRTC] listener opened on :8889 (HTTP), :8189 (ICE/UDP), :8189 (ICE/TCP)
[90m2025/12/29 15:17:11 [0m[32mINF[0m [SRT] listener opened on :8890 (UDP)
[90m2025/12/29 15:17:24 [0m[32mINF[0m [SRT] [conn [::1]:60331] opened
[90m2025/12/29 15:17:26 [0m[32mINF[0m [SRT] [conn [::1]:60331] is publishing to path 'live/sabado', 2 tracks (H264, MPEG-4 Audio)
[90m2025/12/29 15:17:26 [0m[1;33mWAR[0m [path live/sabado] SRT forwarder error: failed to connect: connection rejected: REJECT (va)
[90m2025/12/29 15:17:28 [0m[1;33mWAR[0m [path live/sabado] SRT forwarder error: failed to connect: connection rejected: REJECT (va)
[90m2025/12/29 15:17:30 [0m[1;33mWAR[0m [path live/sabado] SRT forwarder error: failed to connect: connection rejected: REJECT (va)
[90m2025/12/29 15:17:32 [0m[1;33mWAR[0m [path live/sabado] SRT forwarder error: failed to connect: connection rejected: REJECT (va)

D:\download\p2pcdnall\mmx.git\bin>mmx.exe mediamtx_edge.yml
[90m2025/12/29 15:19:35 [0m[32mINF[0m MediaMTX v1.15.5
[90m2025/12/29 15:19:35 [0m[32mINF[0m configuration loaded from D:\download\p2pcdnall\mmx.git\bin\mediamtx_edge.yml
[90m2025/12/29 15:19:35 [0m[32mINF[0m [RTSP] listener opened on :8564 (TCP), :8010 (UDP/RTP), :8011 (UDP/RTCP)
[90m2025/12/29 15:19:35 [0m[32mINF[0m [RTMP] listener opened on :1945
[90m2025/12/29 15:19:35 [0m[32mINF[0m [WebRTC] listener opened on :8899 (HTTP), :8199 (ICE/UDP), :8199 (ICE/TCP)
[90m2025/12/29 15:19:35 [0m[32mINF[0m [SRT] listener opened on :8900 (UDP)
[90m2025/12/29 15:19:36 [0m[32mINF[0m [WebRTC] [session 99e5f737] created by 127.0.0.1:59861
[90m2025/12/29 15:19:36 [0m[32mINF[0m [WebRTC] [session 99e5f737] closed: no stream is available on path 'live/sabado'
[90m2025/12/29 15:19:39 [0m[32mINF[0m [SRT] [conn 127.0.0.1:62300] opened
[90m2025/12/29 15:19:39 [0m[32mINF[0m [SRT] [conn 127.0.0.1:62300] closed: invalid path name: can contain only alphanumeric characters, underscore, dot, tilde, minus, slash, colon ($MTX_PATH)
[90m2025/12/29 15:19:39 [0m[32mINF[0m [WebRTC] [session ea445d88] created by 127.0.0.1:59861
[90m2025/12/29 15:19:39 [0m[32mINF[0m [WebRTC] [session ea445d88] closed: no stream is available on path 'live/sabado'
[90m2025/12/29 15:19:41 [0m[32mINF[0m [SRT] [conn 127.0.0.1:51713] opened
[90m2025/12/29 15:19:41 [0m[32mINF[0m [SRT] [conn 127.0.0.1:51713] closed: invalid path name: can contain only alphanumeric characters, underscore, dot, tilde, minus, slash, colon ($MTX_PATH)

2. 时延偏大，达到6s以上了。
测试组网: obs推SRT到origin节点(本机)，origin转发edge节点(本机)，从edge拉流webrtc。
请帮忙检查一下是否配置原因。
>>
时延还是很大，但是不能再降低writeQueueSize了。
我感觉是mediamtx演示webrtc拉流播放器过于简单造成的。请再分析一下原因
>>即使我从origin node上拉流ffplay "srt://127.0.0.1:8900?streamid=read:live/sabado3"
时延也有7s。为何这么大？代码本身的原因吗？

在 OBS 中将关键帧间隔设置为 1 秒。writeQueueSize = 16
重启 OBS 推流，重新测试SRT拉流延迟还是有6s
我现在只是测试origin node，时延一直很大，无法达到

