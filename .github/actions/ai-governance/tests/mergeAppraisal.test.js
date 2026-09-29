const {
  appraiseAutoMerge,
  findSensitivePath,
  DEFAULT_MAX_FILES
} = require('../src/services/mergeAppraisal');

const gov = { enableAutoApprove: true };
const ok = { detection: 'KEEP', files: [{ filename: 'internal/scheduler/jitter.go' }], totalChanges: 20, draft: false, gov };

describe('findSensitivePath', () => {
  test('CI / 构建 / 依赖 / 脚本类路径命中', () => {
    const hits = [
      '.github/workflows/pr-ci.yml',
      '.github/actions/x/action.yml',
      'Dockerfile',
      'docker-compose.yml',
      'go.mod',
      'go.sum',
      'scripts/task_runner.py'.replace('task_runner.py', 'run.sh'),
      'login.sh',
      'start.cmd',
      'sub/Makefile'
    ];
    hits.forEach(filename => {
      expect(findSensitivePath([{ filename }])).not.toBeNull();
    });
  });

  test('普通源码路径不命中', () => {
    ['internal/scheduler/jitter.go', 'cmd/server/main.go', 'README.md', 'config.example.json'].forEach(filename => {
      expect(findSensitivePath([{ filename }])).toBeNull();
    });
  });

  test('scripts/ 目录下任意文件都命中（不看扩展名）', () => {
    expect(findSensitivePath([{ filename: 'scripts/task_common.py' }])).not.toBeNull();
  });
});

describe('appraiseAutoMerge', () => {
  test('全部条件满足 → 批准', () => {
    expect(appraiseAutoMerge(ok)).toMatchObject({ eligible: true });
  });

  test('未启用时不批准（默认关闭）', () => {
    expect(appraiseAutoMerge({ ...ok, gov: {} })).toMatchObject({ eligible: false });
  });

  test('草稿 PR 不批准', () => {
    expect(appraiseAutoMerge({ ...ok, draft: true })).toMatchObject({ eligible: false, reason: expect.stringContaining('草稿') });
  });

  test('分层检测非 KEEP 一律不批准（UNCLEAR / INVALID_COMMIT / MALICIOUS / TRIVIAL / SPAM）', () => {
    ['UNCLEAR', 'INVALID_COMMIT', 'MALICIOUS', 'TRIVIAL', 'SPAM', undefined].forEach(detection => {
      expect(appraiseAutoMerge({ ...ok, detection }).eligible).toBe(false);
    });
  });

  test('改动清单取不到 / 为空 → 不批准（未知即不放行）', () => {
    expect(appraiseAutoMerge({ ...ok, files: null }).eligible).toBe(false);
    expect(appraiseAutoMerge({ ...ok, files: [] }).eligible).toBe(false);
  });

  test('敏感路径一票否决（哪怕其它条件全满足）', () => {
    const verdict = appraiseAutoMerge({
      ...ok,
      files: [{ filename: 'internal/a.go' }, { filename: '.github/workflows/pr-ci.yml' }]
    });
    expect(verdict.eligible).toBe(false);
    expect(verdict.reason).toContain('.github/workflows/pr-ci.yml');
  });

  test('文件数超限不批准，且上限可配置', () => {
    const many = Array.from({ length: DEFAULT_MAX_FILES + 1 }, (_, i) => ({ filename: `internal/f${i}.go` }));
    expect(appraiseAutoMerge({ ...ok, files: many }).eligible).toBe(false);
    // 自定义上限生效：3 个文件在 max=2 时被拒
    expect(appraiseAutoMerge({ ...ok, files: many.slice(0, 3), gov: { ...gov, maxAutoMergeFiles: 2 } }).eligible).toBe(false);
    expect(appraiseAutoMerge({ ...ok, files: many.slice(0, 2), gov: { ...gov, maxAutoMergeFiles: 2 } }).eligible).toBe(true);
  });

  test('改动行数超限不批准，且上限可配置', () => {
    expect(appraiseAutoMerge({ ...ok, totalChanges: 5000 }).eligible).toBe(false);
    expect(appraiseAutoMerge({ ...ok, totalChanges: 5, gov: { ...gov, maxAutoMergeChanges: 4 } }).eligible).toBe(false);
  });
});
