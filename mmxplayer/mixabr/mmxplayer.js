// mmxplayer.js

// --- ABR 算法参数配置 ---
const ABR_CONFIG = {
    EMA_ALPHA: 0.15, 
    
    DOWNGRADE_LOSS: 0.10,      
    DOWNGRADE_RTT: 1500,       
    DOWNGRADE_FPS: 10,         
    DOWNGRADE_PERSISTENCE: 4,  

    UPGRADE_LOSS: 0.05,        
    UPGRADE_RTT: 1000,         
    UPGRADE_FPS: 10,           
    STABLE_DURATION: 5,        
    
    BASE_PENALTY_TIME: 60000,  
    MAX_PENALTY_TIME: 300000,  
    
    SWITCH_COOLDOWN: 8,        
    HONEYMOON_TIME: 20000      
};

// --- 流定义 ---
const STREAM_VARIANTS = {
    hevc1080: { id: '1080p', label: '1080P (HEVC)', stream: 'hevc1080v', bitrate: 1000, codec: 'hevc' },
    h2641080: { id: '1080p', label: '1080P (H264)', stream: 'd1080v',    bitrate: 2000, codec: 'h264' },
    
    hevc720:  { id: '720p',  label: '720P (HEVC)',  stream: 'hevc720v',  bitrate: 400,  codec: 'hevc' },
    h264720:  { id: '720p',  label: '720P (H264)',  stream: 'd720v',     bitrate: 1000, codec: 'h264' },
    
    h264540:  { id: '540p',  label: '540P (Low)',   stream: 'd540v',     bitrate: 400,  codec: 'h264' },
    audio:    { id: 'audio', label: 'Audio Only',   stream: 'audiov',    bitrate: 64,   codec: 'opus' }
};

/* ------------------------
   HEVC 能力检测模块
   ------------------------ */
let _hevcSupportCached = null; 

async function detectHevcCapability() {
  if (_hevcSupportCached !== null) return _hevcSupportCached;
  let supported = false;
  if (navigator.mediaCapabilities && navigator.mediaCapabilities.decodingInfo) {
    try {
      const info = await navigator.mediaCapabilities.decodingInfo({
        type: "file",
        video: {
          contentType: 'video/mp4; codecs="hvc1.1.6.L93.B0"',
          width: 1280, height: 720, bitrate: 2000000, framerate: 30,
        },
      });
      supported = info.supported;
    } catch (e) {
      supported = internalCanPlayHevc();
    }
  } else {
    supported = internalCanPlayHevc();
  }
  _hevcSupportCached = supported;
  console.log(`[Capability] HEVC Support: ${_hevcSupportCached}`);
  return _hevcSupportCached;
}

function internalCanPlayHevc() {
  const v = document.createElement("video");
  return v.canPlayType('video/mp4; codecs="hvc1"').replace(/^no$/, '') !== '';
}

/* ------------------------
   MMXPlayer 类
   ------------------------ */
class MMXPlayer {
    constructor(videoElement, statsElement, baseUrl) {
        this.video = videoElement;
        this.statsEl = statsElement;
        this.baseUrl = baseUrl.endsWith('/') ? baseUrl : baseUrl + '/';
        
        this.levels = [];
        this.currentReader = null;
        this.currentLevelIdx = 0; 
        
        this.lastSuccessfulLevelIdx = -1;

        this.isAuto = true;
        this.isSwitching = false;
        
        // 标记当前档位是否使用备用流
        this.activeLevelUsingFallback = false;

        this.abrInterval = null;
        this.lastStats = null;
        
        this.stableCount = 0;
        this.badConditionCount = 0;
        this.switchCooldownTimer = 0;
        this.connectionStartTime = Date.now();
        
        this.penaltyBox = {};
        this.failureCounts = {};
        
        this.fpsHistory = []; 
        this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
        this.rawMetrics = { rtt: 0, packetLoss: 0, bitrate: 0, fps: 0, resolution: '' };
    }

    async start() {
        const hasHevc = await detectHevcCapability();

        this.levels = [];
        if (hasHevc) {
            this.levels = [
                { ...STREAM_VARIANTS.hevc1080, fallback: STREAM_VARIANTS.h2641080 },
                { ...STREAM_VARIANTS.hevc720,  fallback: STREAM_VARIANTS.h264720 },
                { ...STREAM_VARIANTS.h264540 },
                { ...STREAM_VARIANTS.audio }
            ];
        } else {
            this.levels = [
                { ...STREAM_VARIANTS.h2641080 },
                { ...STREAM_VARIANTS.h264720 },
                { ...STREAM_VARIANTS.h264540 },
                { ...STREAM_VARIANTS.audio }
            ];
        }

        console.log("[MMXPlayer] Initialized Levels:", this.levels);

        this.currentLevelIdx = 0;
        // 初始加载时不重置 Fallback，使用默认值
        await this.loadLevel(this.currentLevelIdx);
        this.startStatsMonitor();
    }

    async loadLevel(index) {
        if (index < 0 || index >= this.levels.length) return;
        
        // 【重要】如果正在切换中，且不是为了 Fallback 重试，则阻断
        if (this.isSwitching && this.currentLevelIdx === index && !this.activeLevelUsingFallback) return;

        const level = this.levels[index];
        this.isSwitching = true;

        // 【修正】这里移除了 activeLevelUsingFallback = false 的重置逻辑
        // 避免 Fallback 递归调用时状态丢失

        // 确定实际流配置
        let streamConfig = level;
        if (this.activeLevelUsingFallback && level.fallback) {
            console.log(`[MMXPlayer] ⚠️ Using Fallback Stream for ${level.label}: ${level.fallback.label}`);
            streamConfig = level.fallback;
        } else {
             console.log(`[MMXPlayer] Attempting switch to ${level.label} (Index: ${index})...`);
        }

        const whepUrl = new URL(`live/${streamConfig.stream}/whep`, this.baseUrl).toString();
        let trackHandled = false;

        const newReader = new MediaMTXWebRTCReader({
            url: whepUrl,
            onError: (err) => {
                console.error('[Reader Error]', err);
                
                // --- 1. 尝试同级 Fallback (HEVC -> H264) ---
                if (level.fallback && !this.activeLevelUsingFallback) {
                    console.warn(`[MMXPlayer] ♻️ Primary stream ${level.stream} failed. Retrying with fallback: ${level.fallback.stream}`);
                    
                    this.activeLevelUsingFallback = true; // 标记使用 Fallback
                    this.isSwitching = false; 
                    
                    // 稍微延迟重试，防止调用栈溢出
                    setTimeout(() => this.loadLevel(index), 100); 
                    return;
                }

                // --- 2. 如果 Fallback 也失败，或者没有 Fallback ---
                this.penalizeLevel(index); // 关小黑屋
                this.isSwitching = false;

                // --- 3. 决定下一步 (避免死循环) ---
                
                // 3a. 如果有上一个成功的流，回退回去
                if (this.lastSuccessfulLevelIdx !== -1 && this.lastSuccessfulLevelIdx !== index) {
                    console.warn(`[MMXPlayer] 🔙 Switch failed. Reverting to last good level: ${this.levels[this.lastSuccessfulLevelIdx].label}`);
                    // 回退操作需要重置 Fallback 状态，因为我们要去的是另一个 Level
                    // 但这里不能直接设 false，而是在 manualSwitch/decideQuality 中控制，
                    // 或者这里简单地调用 loadLevel，依靠该 Level 之前的状态
                    
                    // 这里为了安全，回退时我们默认重置 Fallback 偏好，让那个 Level 自己决定
                    // 注意：这可能会导致回到上一个 Level 时也经历一次 Primary -> Fallback 的过程
                    // 如果想优化，可以记录每个 Level 的 fallback 状态，这里暂不复杂化。
                    this.activeLevelUsingFallback = false; 
                    
                    setTimeout(() => this.loadLevel(this.lastSuccessfulLevelIdx), 100);
                    return;
                }

                // 3b. 如果是手动模式且没有上一个成功流，就停止尝试，防止无限弹 Log
                if (!this.isAuto) {
                    console.error("[MMXPlayer] Manual switch failed completely. Stopping retries.");
                    return;
                }

                // 3c. 如果是 Auto 模式且还没成功过，尝试降级
                if (this.isAuto && index < this.levels.length - 1) {
                    console.warn(`[ABR] Initial connection failed, downgrading...`);
                    this.activeLevelUsingFallback = false; // 换 Level 了，重置
                    setTimeout(() => this.loadLevel(index + 1), 200);
                }
            },
            onTrack: (evt) => {
                if (trackHandled) return;
                trackHandled = true;

                this.connectionStartTime = Date.now();
                const stream = evt.streams[0];
                
                const activeLabel = (this.activeLevelUsingFallback && level.fallback) 
                    ? level.fallback.label 
                    : level.label;

                console.log(`[MMXPlayer] ✅ Stream Ready: ${activeLabel}`);
                
                this.lastSuccessfulLevelIdx = index;

                this.video.srcObject = stream;
                this.optimizeForLowLatency(stream);

                this.video.play().catch(e => {
                     if (e.name !== 'AbortError') console.warn('Autoplay warning:', e);
                });

                if (this.currentReader && this.currentReader !== newReader) {
                    this.currentReader.close();
                }
                
                this.currentReader = newReader;
                this.currentLevelIdx = index;
                level.currentEffectiveBitrate = streamConfig.bitrate;

                this.lastStats = null;
                this.fpsHistory = [];
                this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
                this.stableCount = 0;
                this.badConditionCount = 0;
                
                this.isSwitching = false;
                this.switchCooldownTimer = ABR_CONFIG.SWITCH_COOLDOWN; 
            }
        });
    }

    penalizeLevel(index) {
        this.failureCounts[index] = (this.failureCounts[index] || 0) + 1;
        const count = this.failureCounts[index];
        let penaltyTime = ABR_CONFIG.BASE_PENALTY_TIME * Math.pow(2, count - 1);
        if (penaltyTime > ABR_CONFIG.MAX_PENALTY_TIME) penaltyTime = ABR_CONFIG.MAX_PENALTY_TIME;

        const banUntil = Date.now() + penaltyTime;
        this.penaltyBox[index] = banUntil;
        console.warn(`[ABR] 🚫 Banning level ${this.levels[index].label} for ${penaltyTime/1000}s`);
    }

    optimizeForLowLatency(stream) {
        stream.getTracks().forEach(t => t.enabled = true);
        if (this.video.buffered.length > 0) {
            const end = this.video.buffered.end(this.video.buffered.length - 1);
            if (end - this.video.currentTime > 0.5) {
                this.video.currentTime = end - 0.1;
            }
        }
    }

    manualSwitch(levelId) {
        if (levelId === 'auto') {
            this.isAuto = true;
            console.log('[MMXPlayer] Mode: AUTO');
            return;
        }

        this.isAuto = false;
        console.log('[MMXPlayer] Mode: MANUAL');
        
        const idx = this.levels.findIndex(l => l.id === levelId);
        
        if (idx !== -1) {
            // 手动强制切换时：
            // 1. 清除惩罚
            this.failureCounts[idx] = 0;
            delete this.penaltyBox[idx];
            
            // 2. 【关键】重置 Fallback 状态，因为用户意图是“我要看这个清晰度”，
            //    所以我们从该清晰度的 Primary 流（如 HEVC）开始尝试。
            this.activeLevelUsingFallback = false; 
            
            this.loadLevel(idx);
        }
    }

    startStatsMonitor() {
        if (this.abrInterval) clearInterval(this.abrInterval);

        this.abrInterval = setInterval(async () => {
            if (this.switchCooldownTimer > 0) this.switchCooldownTimer--;
            if (this.isSwitching || !this.currentReader || !this.currentReader.pc) return;

            try {
                const stats = await this.currentReader.pc.getStats();
                let activeCandidatePair = null;
                let videoRtp = null;
                let audioRtp = null;

                stats.forEach(report => {
                    if (report.type === 'candidate-pair' && report.state === 'succeeded') {
                        activeCandidatePair = report;
                    }
                    if (report.type === 'inbound-rtp') {
                        if (report.kind === 'video') videoRtp = report;
                        if (report.kind === 'audio') audioRtp = report;
                    }
                });

                if (activeCandidatePair) {
                    this.processStats(activeCandidatePair, videoRtp, audioRtp);
                    this.renderStats();
                    
                    if (this.isAuto && this.switchCooldownTimer === 0) {
                        this.decideQuality();
                    }
                }
            } catch (e) {
                console.warn("Stats error", e);
            }
        }, 1000);
    }

    processStats(pair, videoRtp, audioRtp) {
        const now = Date.now();
        const currentIdx = this.currentLevelIdx;
        const isAudioOnlyMode = (this.levels[currentIdx].id === 'audio');

        let primaryRtp = isAudioOnlyMode ? audioRtp : (videoRtp || audioRtp);
        if (!primaryRtp) {
            primaryRtp = { packetsLost: 0, packetsReceived: 0, bytesReceived: 0 };
        }

        const packetsLost = primaryRtp.packetsLost || 0;
        const packetsReceived = primaryRtp.packetsReceived || 0;
        const bytesReceived = primaryRtp.bytesReceived || 0;
        
        let instantLoss = 0;
        let instantBitrate = 0;
        let avgFPS = 30; 

        if (this.lastStats) {
            const deltaLost = packetsLost - this.lastStats.packetsLost;
            const deltaReceived = packetsReceived - this.lastStats.packetsReceived;
            const totalDelta = deltaLost + deltaReceived;
            
            if (totalDelta > 0) instantLoss = Math.max(0, deltaLost / totalDelta);

            const deltaBytes = bytesReceived - this.lastStats.bytesReceived;
            instantBitrate = (deltaBytes * 8) / 1000;

            if (isAudioOnlyMode) {
                this.fpsHistory = [];
                avgFPS = 30;
            } else if (videoRtp) {
                const framesDecoded = videoRtp.framesDecoded || 0;
                this.fpsHistory.push({ t: now, f: framesDecoded });
                
                const windowStart = now - 3000;
                this.fpsHistory = this.fpsHistory.filter(record => record.t >= windowStart);

                if (this.fpsHistory.length > 1) {
                    const first = this.fpsHistory[0];
                    const last = this.fpsHistory[this.fpsHistory.length - 1];
                    const timeDiff = (last.t - first.t) / 1000;
                    const frameDiff = last.f - first.f;
                    if (timeDiff > 0) avgFPS = frameDiff / timeDiff;
                }
            } else {
                this.fpsHistory = [];
                avgFPS = 0;
            }
        } else {
            avgFPS = isAudioOnlyMode ? 30 : 0;
        }

        const instantRtt = (pair.currentRoundTripTime || 0) * 1000;

        if (this.emaMetrics.rtt === 0) {
            this.emaMetrics.rtt = instantRtt;
            this.emaMetrics.packetLoss = instantLoss;
            this.emaMetrics.fps = avgFPS;
        } else {
            this.emaMetrics.rtt = (ABR_CONFIG.EMA_ALPHA * instantRtt) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.rtt);
            this.emaMetrics.packetLoss = (ABR_CONFIG.EMA_ALPHA * instantLoss) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.packetLoss);
            this.emaMetrics.fps = (ABR_CONFIG.EMA_ALPHA * avgFPS) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.fps);
        }

        this.rawMetrics = {
            rtt: instantRtt,
            packetLoss: instantLoss,
            bitrate: instantBitrate,
            fps: avgFPS,
            resolution: (videoRtp && videoRtp.frameWidth) ? `${videoRtp.frameWidth}x${videoRtp.frameHeight}` : 'Audio/None'
        };

        this.lastStats = { 
            packetsLost, packetsReceived, bytesReceived, 
            framesDecoded: (videoRtp ? (videoRtp.framesDecoded || 0) : 0),
            timestamp: now 
        };
    }

    decideQuality() {
        const { rtt, packetLoss, fps } = this.emaMetrics;
        const currentBitrate = this.rawMetrics.bitrate; 
        const currentIdx = this.currentLevelIdx;
        const levelConfig = this.levels[currentIdx];
        const isAudioOnly = (levelConfig.id === 'audio');

        const activeTargetBitrate = levelConfig.currentEffectiveBitrate || levelConfig.bitrate;
        const connectionDuration = Date.now() - this.connectionStartTime;
        const isHoneymoon = connectionDuration < ABR_CONFIG.HONEYMOON_TIME;

        let triggerReason = null;

        if (packetLoss > ABR_CONFIG.DOWNGRADE_LOSS) triggerReason = `Loss(${packetLoss.toFixed(2)})`;
        else if (rtt > ABR_CONFIG.DOWNGRADE_RTT) triggerReason = `RTT(${rtt.toFixed(0)})`;
        else if (!isAudioOnly && !isHoneymoon && fps < ABR_CONFIG.DOWNGRADE_FPS) triggerReason = `FPS(${fps.toFixed(1)})`;
        else if (!isAudioOnly && !isHoneymoon && currentBitrate > 0 && currentBitrate < activeTargetBitrate * 0.7) triggerReason = `BW(${currentBitrate.toFixed(0)})`;

        const isVideoFrozen = !isAudioOnly && !isHoneymoon && (fps <= 1 || this.rawMetrics.fps <= 1);
        const isCritical = (this.rawMetrics.rtt > 3000) || isVideoFrozen;

        if (currentIdx < this.levels.length - 1) {
            if (triggerReason || isCritical) {
                if (isCritical) {
                    console.warn(`[ABR] 🚨 CRITICAL detected. Emergency Downgrade!`);
                    
                    // ABR 决定降级时，重置 Fallback 状态，尝试新 Level 的 Primary
                    this.activeLevelUsingFallback = false;
                    this.loadLevel(currentIdx + 1);
                    this.badConditionCount = 0;
                    return;
                }

                this.badConditionCount++;
                console.debug(`[ABR] ⚠️ Bad condition: ${triggerReason}. Count: ${this.badConditionCount}/${ABR_CONFIG.DOWNGRADE_PERSISTENCE}`);
                
                if (this.badConditionCount >= ABR_CONFIG.DOWNGRADE_PERSISTENCE) {
                    console.warn(`[ABR] 📉 Network poor (${triggerReason}). Downgrading...`);
                    // ABR 决定降级时，重置 Fallback 状态
                    this.activeLevelUsingFallback = false;
                    this.loadLevel(currentIdx + 1);
                    return;
                }
            } else {
                this.badConditionCount = 0;
            }
        }

        if (currentIdx > 0) {
            const targetIdx = currentIdx - 1;
            if (this.isLevelBanned(targetIdx)) {
                this.stableCount = 0;
                return;
            }

            const isFpsHealthy = isAudioOnly || (fps >= ABR_CONFIG.UPGRADE_FPS);
            const isGoodNetwork = 
                (packetLoss <= ABR_CONFIG.UPGRADE_LOSS) && 
                (rtt < ABR_CONFIG.UPGRADE_RTT) &&
                isFpsHealthy;

            if (isGoodNetwork) {
                this.stableCount++;
                if (this.stableCount >= ABR_CONFIG.STABLE_DURATION) {
                    console.log(`[ABR] 📈 Network stable. Upgrading to ${this.levels[targetIdx].label}...`);
                    // ABR 决定升级时，重置 Fallback 状态
                    this.activeLevelUsingFallback = false;
                    this.loadLevel(targetIdx);
                    this.stableCount = 0;
                }
            } else {
                this.stableCount = 0;
            }
        }
    }

    isLevelBanned(index) {
        const banUntil = this.penaltyBox[index];
        if (!banUntil) return false;
        
        if (Date.now() < banUntil) {
            return true;
        } else {
            delete this.penaltyBox[index];
            console.log(`[ABR] ✅ Level ${this.levels[index].label} penalty expired.`);
            return false;
        }
    }

    renderStats() {
        if (!this.statsEl || !this.levels.length) return;
        const level = this.levels[this.currentLevelIdx];
        
        const playingLabel = (this.activeLevelUsingFallback && level.fallback) ? level.fallback.label : level.label;
        const codecLabel = (this.activeLevelUsingFallback && level.fallback) ? level.fallback.codec : (level.codec || 'h264');
        
        const mode = this.isAuto ? `<span style="color:#0f0">AUTO</span>` : `<span style="color:#fa0">MANUAL</span>`;
        
        let bannedInfo = '';
        Object.keys(this.penaltyBox).forEach(idx => {
            const left = Math.ceil((this.penaltyBox[idx] - Date.now()) / 1000);
            if (left > 0 && this.levels[idx]) {
                const fails = this.failureCounts[idx] || 0;
                bannedInfo += `<div style="color:red;font-size:12px">🚫 ${this.levels[idx].id} banned: ${left}s</div>`;
            }
        });

        const downgradeProgress = this.isAuto && this.currentLevelIdx < this.levels.length-1 
            ? `(Bad: ${this.badConditionCount}/${ABR_CONFIG.DOWNGRADE_PERSISTENCE})` : '';
        const upgradeProgress = this.isAuto && this.currentLevelIdx > 0 
            ? `(Stable: ${this.stableCount}/${ABR_CONFIG.STABLE_DURATION})` : '';

        const fpsColor = (level.id !== 'audio' && this.rawMetrics.fps < 10) ? 'red' : 'white';
        const honeymoon = (Date.now() - this.connectionStartTime < ABR_CONFIG.HONEYMOON_TIME) ? '🛡️' : '';

        this.statsEl.innerHTML = `
            <div><strong>Mode:</strong> ${mode} ${honeymoon}</div>
            <div><strong>Stream:</strong> ${playingLabel} <span style="font-size:11px;color:#aaa">(${codecLabel})</span></div>
            <div><strong>Bitrate:</strong> ${this.rawMetrics.bitrate.toFixed(0)} kbps</div>
            <hr style="border-color:#555">
            <div><strong>FPS (3s Avg):</strong> <span style="color:${fpsColor}">${this.rawMetrics.fps.toFixed(1)}</span></div>
            <div><strong>RTT (EMA):</strong> ${this.emaMetrics.rtt.toFixed(0)} ms</div>
            <div><strong>Loss (EMA):</strong> ${(this.emaMetrics.packetLoss * 100).toFixed(2)}%</div>
            <div><strong>Trend:</strong> <span style="font-size:12px">${downgradeProgress} ${upgradeProgress}</span></div>
            ${bannedInfo}
        `;
    }

    destroy() {
        if (this.abrInterval) clearInterval(this.abrInterval);
        if (this.currentReader) this.currentReader.close();
    }
}