// --- START OF FILE reader.js ---
'use strict';

class MediaMTXWebRTCReader {
  constructor(conf) {
    this.retryPause = 2000;
    this.conf = conf;
    this.state = 'getting_codecs';
    this.restartTimeout = null;
    this.pc = null;
    this.offerData = null;
    this.sessionUrl = null;
    this.queuedCandidates = []; // 暂存 ICE 候选者
    this.nonAdvertisedCodecs = [];
    this.#getNonAdvertisedCodecs();
  }

  close() {
    this.state = 'closed';
    if (this.pc !== null) {
      this.pc.close();
      this.pc = null;
    }
    if (this.restartTimeout !== null) {
      clearTimeout(this.restartTimeout);
      this.restartTimeout = null;
    }
    
    // 清理状态，但不发送 DELETE
    this.sessionUrl = null;
    this.offerData = null;
    this.queuedCandidates = [];
  }

  static #limitBandwidth(sdp, bitrate) {
    if (!bitrate || parseInt(bitrate) <= 0) return sdp;
    const lines = sdp.split('\r\n');
    let videoIndex = -1;
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].startsWith('m=video')) {
        videoIndex = i;
        break;
      }
    }
    if (videoIndex !== -1) {
      lines.splice(videoIndex + 1, 0, `b=AS:${bitrate}`);
      console.log(`[MediaMTX] Added bandwidth limit: ${bitrate} kbps`);
    }
    return lines.join('\r\n');
  }

  static #linkToIceServers(links) {
    return (links !== null) ? links.split(', ').map((link) => {
      const m = link.match(/^<(.+?)>; rel="ice-server"(; username="(.*?)"; credential="(.*?)"; credential-type="password")?/i);
      const ret = { urls: [m[1]] };
      if (m[3] !== undefined) {
        ret.username = JSON.parse(`"${m[3]}"`);
        ret.credential = JSON.parse(`"${m[4]}"`);
        ret.credentialType = 'password';
      }
      return ret;
    }) : [];
  }

  static #parseOffer(sdp) {
    const ret = { iceUfrag: '', icePwd: '', medias: [] };
    for (const line of sdp.split('\r\n')) {
      if (line.startsWith('m=')) {
        ret.medias.push(line.slice('m='.length));
      } else if (ret.iceUfrag === '' && line.startsWith('a=ice-ufrag:')) {
        ret.iceUfrag = line.slice('a=ice-ufrag:'.length);
      } else if (ret.icePwd === '' && line.startsWith('a=ice-pwd:')) {
        ret.icePwd = line.slice('a=ice-pwd:'.length);
      }
    }
    return ret;
  }

  static #generateSdpFragment(od, candidates) {
    let frag = `a=ice-ufrag:${od.iceUfrag}\r\n` + `a=ice-pwd:${od.icePwd}\r\n`;
    for (const candidate of candidates) {
      frag += `m=${od.medias[candidate.sdpMLineIndex]}\r\na=mid:${candidate.sdpMLineIndex}\r\na=${candidate.candidate}\r\n`;
    }
    return frag;
  }

  #handleError(err) {
    if (this.state === 'running') {
      // 这里的 close 只是为了重试前的清理，不发 DELETE 是对的
      if (this.pc !== null) {
        this.pc.close();
        this.pc = null;
      }
      this.offerData = null;
      this.sessionUrl = null;
      this.queuedCandidates = [];
      
      this.state = 'restarting';
      this.restartTimeout = window.setTimeout(() => {
        this.restartTimeout = null;
        this.state = 'running';
        this.#start();
      }, this.retryPause);
      
      if (this.conf.onError !== undefined) {
        this.conf.onError(`${err}, retrying in some seconds`);
      }
    } else if (this.state === 'getting_codecs') {
      this.state = 'failed';
      if (this.conf.onError !== undefined) {
        this.conf.onError(err);
      }
    }
  }

  #getNonAdvertisedCodecs() {
    this.state = 'running';
    this.#start();
  }

  #start() {
    this.#requestICEServers()
      .then((iceServers) => this.#setupPeerConnection(iceServers))
      .then((offer) => this.#sendOffer(offer))
      .then((answer) => this.#setAnswer(answer))
      .catch((err) => {
        console.error(err);
        this.#handleError(err.toString());
      });
  }

  #authHeader() {
    if (this.conf.user) return {'Authorization': `Basic ${btoa(`${this.conf.user}:${this.conf.pass}`)}`};
    if (this.conf.token) return {'Authorization': `Bearer ${this.conf.token}`};
    return {};
  }

  #requestICEServers() {
    return fetch(this.conf.url, {
      method: 'OPTIONS',
      headers: { ...this.#authHeader() },
    }).then((res) => MediaMTXWebRTCReader.#linkToIceServers(res.headers.get('Link')));
  }

  #setupPeerConnection(iceServers) {
    if (this.state !== 'running') throw new Error('closed');

    this.pc = new RTCPeerConnection({
      iceServers,
      sdpSemantics: 'unified-plan',
    });

    const direction = 'recvonly';
    this.pc.addTransceiver('video', { direction });
    this.pc.addTransceiver('audio', { direction });

    this.pc.onicecandidate = (evt) => this.#onLocalCandidate(evt);
    this.pc.onconnectionstatechange = () => this.#onConnectionState();
    this.pc.ontrack = (evt) => this.#onTrack(evt);

    return this.pc.createOffer()
      .then((offer) => {
        let sdp = offer.sdp;
        if (this.conf.maxBitrate) {
          sdp = MediaMTXWebRTCReader.#limitBandwidth(sdp, this.conf.maxBitrate);
        }
        const lines = sdp.split('\r\n').filter(l => 
          !l.toLowerCase().startsWith('a=rid:') && 
          !l.toLowerCase().startsWith('a=simulcast:')
        );
        sdp = lines.join('\r\n');

        this.offerData = MediaMTXWebRTCReader.#parseOffer(sdp);
        
        const modifiedOffer = new RTCSessionDescription({ type: 'offer', sdp: sdp });
        return this.pc.setLocalDescription(modifiedOffer).then(() => sdp);
      });
  }

  #sendOffer(offer) {
    if (this.state !== 'running') throw new Error('closed');
    return fetch(this.conf.url, {
      method: 'POST',
      headers: { ...this.#authHeader(), 'Content-Type': 'application/sdp' },
      body: offer,
    }).then((res) => {
      if (res.status !== 201) throw new Error(`bad status code ${res.status}`);
      this.sessionUrl = new URL(res.headers.get('location'), this.conf.url).toString();
      
      // === 【核心修复】 ===
      // 在拿到 sessionUrl 后，立即把刚才积压的 ICE Candidates 发送出去
      if (this.queuedCandidates.length > 0) {
        console.log(`[MediaMTX] Flushing ${this.queuedCandidates.length} queued candidates`);
        this.#sendLocalCandidates(this.queuedCandidates);
        this.queuedCandidates = [];
      }
      // ==================

      return res.text();
    });
  }

  #setAnswer(answer) {
    if (this.state !== 'running') throw new Error('closed');
    return this.pc.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: answer }));
  }

  #onLocalCandidate(evt) {
    if (this.state !== 'running') return;
    if (evt.candidate !== null) {
      // 如果 sessionUrl 还没回来，先存队列
      if (this.sessionUrl === null) {
        this.queuedCandidates.push(evt.candidate);
      } else {
        // 如果 sessionUrl 已经有了，直接发
        this.#sendLocalCandidates([evt.candidate]);
      }
    }
  }

  #sendLocalCandidates(candidates) {
    fetch(this.sessionUrl, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/trickle-ice-sdpfrag', 'If-Match': '*' },
      body: MediaMTXWebRTCReader.#generateSdpFragment(this.offerData, candidates),
    }).catch((err) => {
        // PATCH 失败不一定致命，可能是 Session 关闭了
        console.warn("Candidate patch failed:", err);
        // this.#handleError(err.toString()); // 可选：不立即重启，防止网络波动导致无限重启
    });
  }

  #onConnectionState() {
    if (this.state !== 'running') return;
    if (this.pc.connectionState === 'failed' || this.pc.connectionState === 'closed') {
      this.#handleError('peer connection closed');
    }
  }

  #onTrack(evt) {
    if (this.conf.onTrack !== undefined) this.conf.onTrack(evt);
  }
}
window.MediaMTXWebRTCReader = MediaMTXWebRTCReader;
// --- END OF FILE reader.js ---