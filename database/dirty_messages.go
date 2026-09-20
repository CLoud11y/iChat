package database

import "errors"

const DirtyMessageBatchSize = 128

var ErrDirtyMessagePrefixMismatch = errors.New("dirty message queue prefix changed")

// Compare every raw byte before trimming anything. LRANGE is bounded by the
// validated argument count; no message decoding or database work runs in Lua.
const acknowledgeDirtyPrefix = `
local head = redis.call('LRANGE', KEYS[1], 0, #ARGV - 1)
if #head ~= #ARGV then
    return 0
end
for i = 1, #ARGV do
    if head[i] ~= ARGV[i] then
        return 0
    end
end
redis.call('LTRIM', KEYS[1], #ARGV, -1)
return 1
`
