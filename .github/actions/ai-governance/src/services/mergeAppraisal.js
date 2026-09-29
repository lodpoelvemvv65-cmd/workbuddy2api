/**
 * 自动合并资格评定：AI 判定 + 确定性闸门。
 *
 * 背景：AI 治理从不合并 PR —— 合并一直是维护者动作。本模块给出一条**受闸门约束**的
 * 自动化通路：只有全部条件满足才打 `ai-approved` 标签，再由仓库的原生 auto-merge 在
 * CI（PR CI 工作流的 test 检查）通过后以 squash 合并。
 *
 * 设计原则与治理体系一致 —— **AI 负责判定，脚本负责闸门；任何一项不确定就不批准**：
 *   - 上游分层检测必须是 KEEP（等价于 NOT_SPAM + 标题规范 VALID + 质量 VALID）；
 *   - 改动文件清单必须真的取到（取不到就退出，绝不「未知即放行」）；
 *   - 草稿 PR 不批准；
 *   - 改动规模有上限（大改动留给人工）；
 *   - **敏感路径一票否决**：CI / 工作流 / 构建 / 依赖 / 脚本类文件绝不自动合并 ——
 *     这类改动一旦被自动合并，等于把仓库的执行权限交给外部贡献者。
 *
 * 注意这是「闸门」而非「判据」：它只否决，不替代人工判断；`ai-approved` 标签同时是
 * 可审计记录与人工介入点（摘掉标签 + 关闭 auto-merge 即可叫停）。
 */

// 一票否决的路径：这些文件被自动合并的代价 = 仓库执行权限失守，必须人工过目。
const SENSITIVE_PATH_PATTERNS = [
  { pattern: /^\.github\//, label: 'CI / 工作流 / Action' },
  { pattern: /(^|\/)Dockerfile$/, label: '构建定义' },
  { pattern: /(^|\/)docker-compose\.ya?ml$/, label: '构建定义' },
  { pattern: /^go\.(mod|sum)$/, label: '依赖与构建图' },
  { pattern: /(^|\/)Makefile$/, label: '构建定义' },
  { pattern: /\.(sh|bash|zsh|cmd|ps1)$/, label: '脚本' },
  { pattern: /^scripts\//, label: '脚本目录' }
];

// 规模上限：超过就留给人工（没有证据表明大会更大风险，但大会更值得人看）。
const DEFAULT_MAX_FILES = 20;
const DEFAULT_MAX_CHANGES = 800;

/**
 * 找出改动清单里的敏感路径；没有则返回 null。
 * @param {Array<{filename: string}>} files
 * @returns {{filename: string, label: string}|null}
 */
function findSensitivePath(files) {
  for (const file of files) {
    const filename = String(file && file.filename ? file.filename : '');
    const hit = SENSITIVE_PATH_PATTERNS.find(entry => entry.pattern.test(filename));
    if (hit) {
      return { filename, label: hit.label };
    }
  }
  return null;
}

/**
 * 评定某个 PR 是否可以自动合并。
 *
 * @param {Object} params
 * @param {string} params.detection 上游分层检测结论（KEEP / UNCLEAR / INVALID_COMMIT / SPAM / MALICIOUS / TRIVIAL）
 * @param {Array<{filename: string}>|null} params.files 改动文件清单；取不到时传 null（→ 不批准）
 * @param {number} params.totalChanges 改动行数（additions + deletions）
 * @param {boolean} params.draft 是否草稿 PR
 * @param {Object} params.gov 治理参数（enableAutoApprove / maxAutoMergeFiles / maxAutoMergeChanges）
 * @returns {{eligible: boolean, reason: string}}
 */
function appraiseAutoMerge({ detection, files, totalChanges, draft, gov = {} }) {
  if (!gov.enableAutoApprove) {
    return { eligible: false, reason: '自动合并未启用（enable-auto-approve=false）' };
  }
  if (draft) {
    return { eligible: false, reason: '草稿 PR 不自动合并' };
  }
  if (detection !== 'KEEP') {
    return { eligible: false, reason: `分层检测结论为 ${detection}，只有 KEEP 可自动合并` };
  }
  if (!Array.isArray(files) || files.length === 0) {
    return { eligible: false, reason: '未取到改动文件清单（未知即不放行）' };
  }

  const maxFiles = gov.maxAutoMergeFiles || DEFAULT_MAX_FILES;
  const maxChanges = gov.maxAutoMergeChanges || DEFAULT_MAX_CHANGES;
  if (files.length > maxFiles) {
    return { eligible: false, reason: `改动文件数 ${files.length} 超过上限 ${maxFiles}` };
  }
  if (Number.isFinite(totalChanges) && totalChanges > maxChanges) {
    return { eligible: false, reason: `改动行数 ${totalChanges} 超过上限 ${maxChanges}` };
  }

  const sensitive = findSensitivePath(files);
  if (sensitive) {
    return {
      eligible: false,
      reason: `含敏感路径 ${sensitive.filename}（${sensitive.label}），必须人工合并`
    };
  }

  return { eligible: true, reason: '分层检测 KEEP + 无敏感路径 + 规模在限内' };
}

module.exports = {
  appraiseAutoMerge,
  findSensitivePath,
  SENSITIVE_PATH_PATTERNS,
  DEFAULT_MAX_FILES,
  DEFAULT_MAX_CHANGES
};
