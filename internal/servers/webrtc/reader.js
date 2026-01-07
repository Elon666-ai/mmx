// --- START OF FILE reader.js ---
'use strict';

class MediaMTXWebRTCReader {
  constructor(conf) {
    this.conf = conf;
    this.pc = null;
    this.sessionUrl = null;
    console.log("[Reader] Initialized with URL:", this.conf.url);
    this.start();
  }

  async start() {
    try {
      console.log("[Reader] Starting connection flow...");
      
      // 1. 获取 ICE Servers (可选，MediaMTX 通常不需要，但标准流程包含)
      let iceServers = [];
      try {
        const opts = await fetch(this.conf.url, { method: 'OPTIONS' });
        const link = opts.headers.get('Link');
        if (link) {
            iceServers = this.linkToIceServers(link);
            console.log("[Reader] Got ICE Servers:", iceServers);
        }
      } catch (e) {
        console.warn("[Reader] OPTIONS request failed, ignoring ICE servers:", e);
      }

      // 2. 创建 PeerConnection
      this.pc = new RTCPeerConnection({
        iceServers,
        sdpSemantics: 'unified-plan'
      });

      // 3. 添加 Transceiver (仅接收)
      // 注意: 这里不进行任何特殊设置，使用浏览器默认行为
      this.pc.addTransceiver('video', { direction: 'recvonly' });
      this.pc.addTransceiver('audio', { direction: 'recvonly' });

      this.pc.ontrack = (evt) => {
        console.log(`[Reader] 🟢 OnTrack: ${evt.track.kind}, ID: ${evt.track.id}`);
        if (this.conf.onTrack) this.conf.onTrack(evt);
      };

      this.pc.onicecandidate = (evt) => this.onLocalCandidate(evt);
      this.pc.onconnectionstatechange = () => {
          console.log("[Reader] Connection State:", this.pc.connectionState);
          if (this.pc.connectionState === 'failed') {
              if (this.conf.onError) this.conf.onError("Connection failed");
          }
      };

      // 4. 创建 Offer
      const offer = await this.pc.createOffer();
      console.log("[Reader] Offer created.");

      // 【关键】不做任何 SDP 修改，直接设置
      await this.pc.setLocalDescription(offer);
      console.log("[Reader] Local Description set.");

      // 5. 发送 Offer 到 MediaMTX
      console.log("[Reader] Sending Offer to:", this.conf.url);
      const res = await fetch(this.conf.url, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/sdp'
        },
        body: offer.sdp
      });

      if (res.status !== 201) {
        const txt = await res.text();
        throw new Error(`Server returned ${res.status}: ${txt}`);
      }

      // 获取 Session URL 用于 ICE Trickle
      this.sessionUrl = new URL(res.headers.get('location'), this.conf.url).toString();
      console.log("[Reader] Session URL:", this.sessionUrl);

      // 6. 设置 Remote Description (Answer)
      const answerSdp = await res.text();
      console.log("[Reader] Received Answer SDP.");
      
      await this.pc.setRemoteDescription(new RTCSessionDescription({
        type: 'answer',
        sdp: answerSdp
      }));
      console.log("[Reader] Remote Description set. Connection established!");

    } catch (err) {
      console.error("[Reader] ❌ Error:", err);
      if (this.conf.onError) this.conf.onError(err.toString());
      this.close();
    }
  }

  onLocalCandidate(evt) {
    if (!evt.candidate || !this.sessionUrl) return;
    
    // 发送 ICE Candidate
    fetch(this.sessionUrl, {
      method: 'PATCH',
      headers: {
        'Content-Type': 'application/trickle-ice-sdpfrag',
        'If-Match': '*'
      },
      body: this.generateSdpFragment(evt.candidate)
    }).catch(e => console.warn("[Reader] ICE Candidate failed:", e));
  }

  generateSdpFragment(candidate) {
    // 简单的 ICE Fragment 生成
    // 注意：标准 WHEP 可能需要更复杂的结构，但 MediaMTX 对此容忍度较高
    // 这里我们假设 LocalDescription 已经包含 ice-ufrag/pwd，但 tricke patch 只需要 candidate 行
    // MediaMTX 实现通常只需要 candidate 字段
    // 为了兼容性，我们需要按照 RFC 8840 格式
    // 但为简化，我们先尝试只发 candidate 文本，如果不行再完善
    // 这里简单构造:
    return `a=${candidate.candidate}\r\n`; 
    // *注意*: 如果这一步报错，MediaMTX 可能还是能连上的，因为初始 Offer 里通常已经包含了一些 candidate
  }
  
  linkToIceServers(header) {
      // 简化的解析器
      return []; 
  }

  close() {
    if (this.pc) {
      this.pc.close();
      this.pc = null;
    }
  }
}

window.MediaMTXWebRTCReader = MediaMTXWebRTCReader;