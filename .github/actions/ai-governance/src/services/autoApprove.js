const core = require('@actions/core');
const { appraiseAutoMerge } = require('./mergeAppraisal');
const githubOps = require('./github');

/**
 * 自动合并的「执行侧」：过闸门就给 PR 打 ai-approved 标签。
 *
 * 分工（与整个治理体系一致）：
 *   - mergeAppraisal 负责**判定与闸门**（纯函数，可单测）；
 *   - 本模块负责**取数 + 落地写操作**：取 PR 改动清单 → 交给闸门 → 打标签；
 *   - 真正的合并交给 GitHub 原生 auto-merge（.github/workflows/pr-automerge.yml
 *     在 AI 治理运行结束后启用它），CI 通过才会合 —— 所以标签只是「AI 批准」，
 *     合并不由脚本执行，关掉 workflow 或摘掉标签即可叫停。
 *
 * 安全约定：取改动清单失败 → **不批准**（未知即不放行）；打标签失败只告警，
 * 不抛错、不影响治理主链路（fail-soft，与其它写操作同构）。
 *
 * @param {Object} octokit GitHub 客户端
 * @param {string} owner
 * @param {string} repo
 * @param {Object} pr PR（含 number / draft）
 * @param {string} detection 上游分层检测结论
 * @param {Object} gov 治理参数（enableAutoApprove / autoApproveLabel / maxAutoMerge*）
 * @param {Object} config 合并后的配置（取日志文案）
 * @param {Object} ops GitHub 写操作集合（默认 src/services/github.js，测试注入 mock）
 * @returns {Promise<{eligible: boolean, reason: string}>}
 */
async function approveIfEligible(octokit, owner, repo, pr, detection, gov = {}, config = {}, ops = githubOps) {
  if (!gov.enableAutoApprove) {
    // 一定要留下日志：这条路径曾因「输入没透传到 gov」而静默跳过，日志是唯一的取证线索
    core.info(`PR #${pr.number} 不自动合并：自动合并未启用（enable-auto-approve=false）`);
    return { eligible: false, reason: '自动合并未启用（enable-auto-approve=false）' };
  }

  let files = null;
  let totalChanges = 0;
  try {
    const response = await octokit.rest.pulls.listFiles({
      owner,
      repo,
      pull_number: pr.number,
      per_page: 100
    });
    const list = response.data || [];
    files = list.map(file => ({ filename: file.filename }));
    totalChanges = list.reduce((sum, file) => sum + (file.additions || 0) + (file.deletions || 0), 0);
  } catch (error) {
    core.warning(`PR #${pr.number} 读取改动清单失败，按「未知即不放行」处理：${error.message}`);
    files = null;
  }

  const verdict = appraiseAutoMerge({
    detection,
    files,
    totalChanges,
    draft: Boolean(pr.draft),
    gov
  });

  if (!verdict.eligible) {
    core.info(`PR #${pr.number} 不自动合并：${verdict.reason}`);
    return verdict;
  }

  core.info(`PR #${pr.number} 满足自动合并条件（${verdict.reason}），打标签 ${gov.autoApproveLabel}`);
  try {
    await ops.addLabels(octokit, owner, repo, pr.number, [gov.autoApproveLabel], config.logging ? config.logging.label_add_api_failed : '');
  } catch (error) {
    core.warning(`PR #${pr.number} 打自动合并标签失败（不影响治理）：${error.message}`);
  }
  return verdict;
}

module.exports = { approveIfEligible };
