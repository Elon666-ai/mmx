// mmxplayer.js

// --- ABR 算法参数配置 ---
const ABR_CONFIG = {
    EMA_ALPHA: 0.15, 
    
    DOWNGRADE_LOSS: 0.10,      
    DOWNGRADE_RTT: 500,        // 调低 RTT 阈值，WebRTC 对延迟更敏感
    DOWNGRADE_FPS: 10,         
    DOWNGRADE_PERSISTENCE: 4,  

    UPGRADE_LOSS: 0.02,        
    UPGRADE_RTT: 200,         
    UPGRADE_FPS: 25,           
    STABLE_DURATION: 8,        
    
    BASE_PENALTY_TIME: 30000,  
    MAX_PENALTY_TIME: 120000,  
    
    SWITCH_COOLDOWN: 5,        
    HONEYMOON_TIME: 10000      
};

// --- Simulcast 档位定义 ---
// 注意：maxBitrate 单位为 kbps。如果不设置或设为 0，则不限制（通常为最高层）
const SIMULCAST_LEVELS = [
    { id: 'high',   label: 'High (Original)',  maxBitrate: 0,    expectedBitrate: 3000, codec: 'auto' },
    { id: 'medium', label: 'Medium (SD)',      maxBitrate: 1500, expectedBitrate: 1200, codec: 'auto' },
    { id: 'low',    label: 'Low (Smooth)',     maxBitrate: 500,  expectedBitrate: 400,  codec: 'auto' },
    { id: 'audio',  label: 'Audio Only',       maxBitrate: 64,   expectedBitrate: 64,   codec: 'opus', audioOnly: true }
];

/* ------------------------
   MMXPlayer 类 (Simulcast 版)
   ------------------------ */
class MMXPlayer {
    /**
     * @param {HTMLVideoElement} videoElement 
     * @param {HTMLElement} statsElement 
     * @param {string} simulcastUrl - 完整的 WHEP URL，例如 http://localhost:8899/live/simulcastv/whep
     */
    constructor(videoElement, statsElement, simulcastUrl) {
        this.video = videoElement;
        this.statsEl = statsElement;
        this.simulcastUrl = simulcastUrl;
        
        this.levels = [...SIMULCAST_LEVELS];
        this.currentReader = null;
        this.currentLevelIdx = 0; 
        
        this.isAuto = true;
        this.isSwitching = false;
        
        this.abrInterval = null;
        this.lastStats = null;
        
        this.stableCount = 0;
        this.badConditionCount = 0;
        this.switchCooldownTimer = 0;
        this.connectionStartTime = Date.now();
        
        this.penaltyBox = {};
        this.failureCounts = {};
        
        this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
        this.rawMetrics = { rtt: 0, packetLoss: 0, bitrate: 0, fps: 0, resolution: '' };
    }

    start() {
        console.log("[MMXPlayer] Starting Simulcast Player with URL:", this.simulcastUrl);
        // 默认从最高画质开始，或者可以改为从 Medium (index 1) 开始以加快首屏
        this.currentLevelIdx = 0;
        this.loadLevel(this.currentLevelIdx);
        this.startStatsMonitor();
    }

    async loadLevel(index) {
        if (index < 0 || index >= this.levels.length) return;
        
        // 如果正在切换且不是为了重试，则阻断
        if (this.isSwitching && this.currentLevelIdx === index) return;

        const level = this.levels[index];
        this.isSwitching = true;

        console.log(`[MMXPlayer] 🔄 Switching to level: ${level.label} (MaxBW: ${level.maxBitrate || 'Unlimited'} kbps)`);

        // 停止旧的 Reader
        if (this.currentReader) {
            this.currentReader.close();
            this.currentReader = null;
        }

        const newReader = new MediaMTXWebRTCReader({
            url: this.simulcastUrl,
            // 【关键】传入带宽限制，reader.js 需支持此参数并修改 SDP
            maxBitrate: level.maxBitrate, 
            audioOnly: level.audioOnly,
            onError: (err) => {
                console.error('[Reader Error]', err);
                this.isSwitching = false;
                
                // 简单的自动降级重试逻辑
                if (this.isAuto && index < this.levels.length - 1) {
                    console.warn(`[MMXPlayer] Level ${level.label} failed, trying lower level...`);
                    setTimeout(() => this.loadLevel(index + 1), 500);
                }
            },
            onTrack: (evt) => {
                this.connectionStartTime = Date.now();
                const stream = evt.streams[0];
                
                console.log(`[MMXPlayer] ✅ Connected to ${level.label}`);
                
                this.video.srcObject = stream;
                
                // 只有非纯音频模式才做低延迟优化，避免音频鬼畜
                if (!level.audioOnly) {
                    this.optimizeForLowLatency(stream);
                }

                this.video.play().catch(e => {
                     if (e.name !== 'AbortError') console.warn('Autoplay warning:', e);
                });

                this.currentReader = newReader;
                this.currentLevelIdx = index;

                // 重置统计状态
                this.lastStats = null;
                this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
                this.stableCount = 0;
                this.badConditionCount = 0;
                this.isSwitching = false;
                this.switchCooldownTimer = ABR_CONFIG.SWITCH_COOLDOWN; 
            }
        });
    }

    optimizeForLowLatency(stream) {
        // 对于 Simulcast 切换，可能不需要过于激进的跳帧，否则画面会卡顿
        // 仅保留最基本的播放速度控制
        if (this.video.buffered.length > 0) {
            const end = this.video.buffered.end(this.video.buffered.length - 1);
            if (end - this.video.currentTime > 2.0) {
                // 延迟过大时才跳跃
                this.video.currentTime = end - 0.5;
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
        const level = this.levels[this.currentLevelIdx];
        const isAudioOnly = level.audioOnly;

        let primaryRtp = isAudioOnly ? audioRtp : (videoRtp || audioRtp);
        if (!primaryRtp) primaryRtp = { packetsLost: 0, packetsReceived: 0, bytesReceived: 0 };

        const packetsLost = primaryRtp.packetsLost || 0;
        const packetsReceived = primaryRtp.packetsReceived || 0;
        const bytesReceived = primaryRtp.bytesReceived || 0;
        
        let instantLoss = 0;
        let instantBitrate = 0;
        let fps = 0;

        if (this.lastStats) {
            const deltaLost = packetsLost - this.lastStats.packetsLost;
            const deltaReceived = packetsReceived - this.lastStats.packetsReceived;
            const totalDelta = deltaLost + deltaReceived;
            
            if (totalDelta > 0) instantLoss = Math.max(0, deltaLost / totalDelta);

            const deltaBytes = bytesReceived - this.lastStats.bytesReceived;
            instantBitrate = (deltaBytes * 8) / 1000; // kbps

            if (!isAudioOnly && videoRtp) {
                const framesDecoded = videoRtp.framesDecoded || 0;
                const deltaFrames = framesDecoded - this.lastStats.framesDecoded;
                const deltaTime = (now - this.lastStats.timestamp) / 1000;
                if (deltaTime > 0) fps = deltaFrames / deltaTime;
            } else {
                fps = 30; // Audio only treat as full FPS
            }
        }

        const instantRtt = (pair.currentRoundTripTime || 0) * 1000;

        // EMA 平滑处理
        if (this.emaMetrics.rtt === 0) {
            this.emaMetrics.rtt = instantRtt;
            this.emaMetrics.packetLoss = instantLoss;
            this.emaMetrics.fps = fps;
        } else {
            this.emaMetrics.rtt = (ABR_CONFIG.EMA_ALPHA * instantRtt) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.rtt);
            this.emaMetrics.packetLoss = (ABR_CONFIG.EMA_ALPHA * instantLoss) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.packetLoss);
            this.emaMetrics.fps = (ABR_CONFIG.EMA_ALPHA * fps) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.fps);
        }

        this.rawMetrics = {
            rtt: instantRtt,
            packetLoss: instantLoss,
            bitrate: instantBitrate,
            fps: fps,
            resolution: (videoRtp && videoRtp.frameWidth) ? `${videoRtp.frameWidth}x${videoRtp.frameHeight}` : (isAudioOnly ? 'AudioOnly' : 'N/A')
        };

        this.lastStats = { 
            packetsLost, packetsReceived, bytesReceived, 
            framesDecoded: (videoRtp ? (videoRtp.framesDecoded || 0) : 0),
            timestamp: now 
        };
    }

    decideQuality() {
        const { rtt, packetLoss, fps } = this.emaMetrics;
        const currentIdx = this.currentLevelIdx;
        const levelConfig = this.levels[currentIdx];
        const isHoneymoon = (Date.now() - this.connectionStartTime) < ABR_CONFIG.HONEYMOON_TIME;

        let triggerReason = null;

        // 降级判断
        if (packetLoss > ABR_CONFIG.DOWNGRADE_LOSS) triggerReason = `Loss(${packetLoss.toFixed(2)})`;
        else if (rtt > ABR_CONFIG.DOWNGRADE_RTT) triggerReason = `RTT(${rtt.toFixed(0)})`;
        else if (!levelConfig.audioOnly && !isHoneymoon && fps < ABR_CONFIG.DOWNGRADE_FPS) triggerReason = `FPS(${fps.toFixed(1)})`;

        if (currentIdx < this.levels.length - 1) {
            if (triggerReason) {
                this.badConditionCount++;
                console.debug(`[ABR] ⚠️ Condition poor: ${triggerReason}. Count: ${this.badConditionCount}/${ABR_CONFIG.DOWNGRADE_PERSISTENCE}`);
                
                if (this.badConditionCount >= ABR_CONFIG.DOWNGRADE_PERSISTENCE) {
                    console.warn(`[ABR] 📉 Downgrading to ${this.levels[currentIdx+1].label}`);
                    this.loadLevel(currentIdx + 1);
                    return;
                }
            } else {
                this.badConditionCount = 0;
            }
        }

        // 升级判断
        if (currentIdx > 0) {
            const targetIdx = currentIdx - 1;
            
            // 简单判断网络是否良好
            const isGoodNetwork = 
                (packetLoss <= ABR_CONFIG.UPGRADE_LOSS) && 
                (rtt < ABR_CONFIG.UPGRADE_RTT) &&
                (levelConfig.audioOnly || fps >= ABR_CONFIG.UPGRADE_FPS);

            if (isGoodNetwork) {
                this.stableCount++;
                if (this.stableCount >= ABR_CONFIG.STABLE_DURATION) {
                    console.log(`[ABR] 📈 Upgrading to ${this.levels[targetIdx].label}`);
                    this.loadLevel(targetIdx);
                    this.stableCount = 0;
                }
            } else {
                this.stableCount = 0;
            }
        }
    }

    renderStats() {
        if (!this.statsEl || !this.levels.length) return;
        const level = this.levels[this.currentLevelIdx];
        const mode = this.isAuto ? `<span style="color:#0f0">AUTO</span>` : `<span style="color:#fa0">MANUAL</span>`;
        
        const trendInfo = this.isAuto 
            ? `(D:${this.badConditionCount} U:${this.stableCount})` 
            : '';

        this.statsEl.innerHTML = `
            <div><strong>Mode:</strong> ${mode}</div>
            <div><strong>Level:</strong> ${level.label}</div>
            <div><strong>Target MaxBW:</strong> ${level.maxBitrate ? level.maxBitrate + ' kbps' : 'Unlimited'}</div>
            <div><strong>Current BW:</strong> ${this.rawMetrics.bitrate.toFixed(0)} kbps</div>
            <div><strong>Resolution:</strong> ${this.rawMetrics.resolution}</div>
            <hr style="border-color:#555">
            <div><strong>FPS:</strong> ${this.rawMetrics.fps.toFixed(1)}</div>
            <div><strong>RTT:</strong> ${this.emaMetrics.rtt.toFixed(0)} ms</div>
            <div><strong>Loss:</strong> ${(this.emaMetrics.packetLoss * 100).toFixed(2)}%</div>
            <div style="font-size:12px;color:#aaa">${trendInfo}</div>
        `;
    }

    destroy() {
        if (this.abrInterval) clearInterval(this.abrInterval);
        if (this.currentReader) this.currentReader.close();
    }
}