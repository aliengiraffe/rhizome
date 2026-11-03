#!/usr/bin/env node
// Launcher for the @aliengiraffe/rhizome npm package. It runs the native
// binary from the platform package npm installed as an optional dependency;
// it downloads nothing and the package has no install scripts.
'use strict';

const childProcess = require('child_process');

const RELEASES = 'https://github.com/aliengiraffe/rhizome/releases';

// Keep in step with npmPlatforms in internal/release/npm.go.
const PACKAGES = {
  'darwin arm64': '@aliengiraffe/rhizome-darwin',
  'darwin x64': '@aliengiraffe/rhizome-darwin',
  'linux arm64': '@aliengiraffe/rhizome-linux-arm64',
  'linux x64': '@aliengiraffe/rhizome-linux-x64',
  'win32 arm64': '@aliengiraffe/rhizome-win32-arm64',
  'win32 x64': '@aliengiraffe/rhizome-win32-x64',
};

// binaryFor returns the platform package and the module path of its binary,
// or null when Rhizome publishes no binary for platform/arch.
function binaryFor(platform, arch) {
  const pkg = PACKAGES[platform + ' ' + arch];
  if (!pkg) return null;
  return { pkg, path: pkg + '/bin/' + (platform === 'win32' ? 'rhizome.exe' : 'rhizome') };
}

// resolveBinary returns the binary's absolute path or throws an Error whose
// message tells the user what to install instead.
function resolveBinary(platform, arch, resolve) {
  const target = binaryFor(platform, arch);
  if (!target) {
    throw new Error(
      `rhizome: no prebuilt binary for ${platform}/${arch}.\n` +
        `Download a build from ${RELEASES} or build from source.`,
    );
  }
  try {
    return resolve(target.path);
  } catch {
    throw new Error(
      `rhizome: the platform package ${target.pkg} is not installed.\n` +
        'It is an optional dependency; reinstall without --omit=optional or --no-optional,\n' +
        `for example: npm i -g @aliengiraffe/rhizome, or download a build from ${RELEASES}.`,
    );
  }
}

// exitCode maps a spawnSync result to the launcher's exit status. A signal is
// returned as { signal } so the caller can re-raise it.
function exitCode(result) {
  if (result.error) return { code: 1, message: `rhizome: ${result.error.message}` };
  if (result.signal) return { signal: result.signal };
  return { code: result.status === null ? 1 : result.status };
}

function main() {
  let bin;
  try {
    bin = resolveBinary(process.platform, process.arch, require.resolve);
  } catch (err) {
    process.stderr.write(err.message + '\n');
    process.exit(1);
  }
  const out = exitCode(childProcess.spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' }));
  if (out.signal) {
    process.kill(process.pid, out.signal);
    // Not every signal terminates the launcher (and Windows has none).
    process.exit(1);
  }
  if (out.message) process.stderr.write(out.message + '\n');
  process.exit(out.code);
}

module.exports = { PACKAGES, binaryFor, resolveBinary, exitCode };

if (require.main === module) main();
