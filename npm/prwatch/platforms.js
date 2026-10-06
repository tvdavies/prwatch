"use strict";

// Maps `${process.platform} ${process.arch}` to the npm package holding the
// native binary for that platform.
module.exports = {
  "linux x64": "@tvdavies/prwatch-linux-x64",
  "linux arm64": "@tvdavies/prwatch-linux-arm64",
  "darwin x64": "@tvdavies/prwatch-darwin-x64",
  "darwin arm64": "@tvdavies/prwatch-darwin-arm64",
};
