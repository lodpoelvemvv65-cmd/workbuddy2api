const { approveIfEligible } = require('../src/services/autoApprove');

const CONFIG = { logging: { label_add_api_failed: '打标签失败' } };
const GOV = { enableAutoApprove: true, autoApproveLabel: 'ai-approved' };

function makeOctokit(files = [{ filename: 'internal/scheduler/jitter.go', additions: 10, deletions: 2 }]) {
  return {
    rest: {
      pulls: {
        listFiles: jest.fn().mockResolvedValue({ data: files })
      }
    }
  };
}

describe('approveIfEligible', () => {
  test('关闭时不取改动清单、不打标签', async () => {
    const octokit = makeOctokit();
    const ops = { addLabels: jest.fn() };

    const verdict = await approveIfEligible(octokit, 'o', 'r', { number: 5 }, 'KEEP', {}, CONFIG, ops);

    expect(verdict.eligible).toBe(false);
    expect(octokit.rest.pulls.listFiles).not.toHaveBeenCalled();
    expect(ops.addLabels).not.toHaveBeenCalled();
  });

  test('过闸门 → 打 ai-approved 标签', async () => {
    const octokit = makeOctokit();
    const ops = { addLabels: jest.fn().mockResolvedValue({}) };

    const verdict = await approveIfEligible(octokit, 'o', 'r', { number: 5 }, 'KEEP', GOV, CONFIG, ops);

    expect(verdict.eligible).toBe(true);
    expect(ops.addLabels).toHaveBeenCalledWith(octokit, 'o', 'r', 5, ['ai-approved'], '打标签失败');
  });

  test('敏感路径一票否决：不打标签', async () => {
    const octokit = makeOctokit([{ filename: '.github/workflows/pr-ci.yml', additions: 1, deletions: 0 }]);
    const ops = { addLabels: jest.fn() };

    const verdict = await approveIfEligible(octokit, 'o', 'r', { number: 5 }, 'KEEP', GOV, CONFIG, ops);

    expect(verdict.eligible).toBe(false);
    expect(verdict.reason).toContain('.github/workflows/pr-ci.yml');
    expect(ops.addLabels).not.toHaveBeenCalled();
  });

  test('取改动清单失败 → 不批准（未知即不放行）', async () => {
    const octokit = { rest: { pulls: { listFiles: jest.fn().mockRejectedValue(new Error('boom')) } } };
    const ops = { addLabels: jest.fn() };

    const verdict = await approveIfEligible(octokit, 'o', 'r', { number: 5 }, 'KEEP', GOV, CONFIG, ops);

    expect(verdict.eligible).toBe(false);
    expect(verdict.reason).toContain('未取到改动文件清单');
    expect(ops.addLabels).not.toHaveBeenCalled();
  });

  test('打标签失败不抛错（fail-soft，不影响治理主链路）', async () => {
    const octokit = makeOctokit();
    const ops = { addLabels: jest.fn().mockRejectedValue(new Error('403')) };

    await expect(approveIfEligible(octokit, 'o', 'r', { number: 5 }, 'KEEP', GOV, CONFIG, ops))
      .resolves.toMatchObject({ eligible: true });
  });
});
