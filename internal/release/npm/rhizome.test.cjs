'use strict';

// node --test internal/release/npm/rhizome.test.cjs
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const launcher = require('./rhizome.js');

test('every supported platform maps to its package and binary', () => {
  const want = {
    'darwin arm64': '@aliengiraffe/rhizome-darwin/bin/rhizome',
    'darwin x64': '@aliengiraffe/rhizome-darwin/bin/rhizome',
    'linux arm64': '@aliengiraffe/rhizome-linux-arm64/bin/rhizome',
    'linux x64': '@aliengiraffe/rhizome-linux-x64/bin/rhizome',
    'win32 arm64': '@aliengiraffe/rhizome-win32-arm64/bin/rhizome.exe',
    'win32 x64': '@aliengiraffe/rhizome-win32-x64/bin/rhizome.exe',
  };
  assert.deepEqual(Object.keys(launcher.PACKAGES).sort(), Object.keys(want).sort());
  for (const [key, modulePath] of Object.entries(want)) {
    const [platform, arch] = key.split(' ');
    assert.equal(launcher.binaryFor(platform, arch).path, modulePath);
  }
});

test('unsupported platforms get an actionable error', () => {
  for (const [platform, arch] of [['freebsd', 'x64'], ['linux', 'ia32'], ['darwin', 'ppc64'], ['win32', 'ia32']]) {
    assert.equal(launcher.binaryFor(platform, arch), null);
    assert.throws(
      () => launcher.resolveBinary(platform, arch, () => assert.fail('resolve called')),
      (err) => err.message.includes(`${platform}/${arch}`) && err.message.includes('github.com/aliengiraffe/rhizome/releases'),
    );
  }
});

test('a missing platform package names the package to install', () => {
  const missing = () => {
    throw Object.assign(new Error('Cannot find module'), { code: 'MODULE_NOT_FOUND' });
  };
  assert.throws(
    () => launcher.resolveBinary('linux', 'arm64', missing),
    (err) =>
      err.message.includes('@aliengiraffe/rhizome-linux-arm64') &&
      err.message.includes('--omit=optional') &&
      err.message.includes('github.com/aliengiraffe/rhizome/releases'),
  );
  assert.equal(launcher.resolveBinary('linux', 'x64', (p) => '/abs/' + p), '/abs/@aliengiraffe/rhizome-linux-x64/bin/rhizome');
});

test('exit codes and signals are forwarded', () => {
  assert.deepEqual(launcher.exitCode({ status: 0, signal: null }), { code: 0 });
  assert.deepEqual(launcher.exitCode({ status: 3, signal: null }), { code: 3 });
  assert.deepEqual(launcher.exitCode({ status: null, signal: 'SIGTERM' }), { signal: 'SIGTERM' });
  assert.equal(launcher.exitCode({ error: new Error('spawn EACCES') }).code, 1);
});

// End to end: a fake platform package next to the launcher, as npm lays it
// out, must receive the arguments and stdin and pass back output and status.
test('the launcher runs the platform binary with args, stdio and exit code', { skip: !launcher.binaryFor(process.platform, process.arch) }, () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'rhizome-launcher-'));
  try {
    const target = launcher.binaryFor(process.platform, process.arch);
    const main = path.join(root, 'node_modules', '@aliengiraffe', 'rhizome', 'bin');
    fs.mkdirSync(main, { recursive: true });
    fs.copyFileSync(path.join(__dirname, 'rhizome.js'), path.join(main, 'rhizome.js'));
    const bin = path.join(root, 'node_modules', ...target.path.split('/'));
    fs.mkdirSync(path.dirname(bin), { recursive: true });
    // The fake binary is node itself on Windows (an .exe is required there),
    // so it runs a script named by the first argument.
    let args = ['a b', '--flag'];
    if (process.platform === 'win32') {
      fs.copyFileSync(process.execPath, bin);
      const script = path.join(root, 'fake.js');
      fs.writeFileSync(script, fakeScript());
      args = [script, ...args];
    } else {
      fs.writeFileSync(bin, `#!${process.execPath}\n${fakeScript()}`, { mode: 0o755 });
    }
    const out = spawnSync(process.execPath, [path.join(main, 'rhizome.js'), ...args], { input: 'from stdin', encoding: 'utf8' });
    assert.equal(out.status, 7, out.stderr);
    assert.match(out.stdout, /args=.*"a b","--flag"\]/);
    assert.match(out.stdout, /stdin=from stdin/);
    assert.equal(out.stderr, 'to stderr\n');

    if (process.platform !== 'win32') {
      // A child killed by a signal kills the launcher with the same signal.
      const killed = spawnSync(process.execPath, [path.join(main, 'rhizome.js')], { input: '', env: { ...process.env, FAKE_SIGNAL: 'SIGTERM' } });
      assert.equal(killed.signal, 'SIGTERM');
    }

    // Without the platform package the launcher explains what is missing.
    fs.rmSync(path.dirname(path.dirname(bin)), { recursive: true });
    const missing = spawnSync(process.execPath, [path.join(main, 'rhizome.js')], { encoding: 'utf8' });
    assert.equal(missing.status, 1);
    assert.match(missing.stderr, new RegExp(target.pkg.replace('/', '\\/')));
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

function fakeScript() {
  return [
    "const input = require('fs').readFileSync(0, 'utf8');",
    'if (process.env.FAKE_SIGNAL) process.kill(process.pid, process.env.FAKE_SIGNAL);',
    "process.stdout.write('args=' + JSON.stringify(process.argv.slice(2)) + '\\nstdin=' + input + '\\n');",
    "process.stderr.write('to stderr\\n');",
    'process.exit(7);',
    '',
  ].join('\n');
}
