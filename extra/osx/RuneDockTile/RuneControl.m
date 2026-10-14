// Copyright (C) 2017-2026 The Rune Authors
// SPDX-License-Identifier: GPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

#import "RuneControl.h"
#import "RuneStatus.h"

#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

NSErrorDomain const RuneControlErrorDomain = @"dev.rune.docktile.control";

NSString *const RuneControlCommandNetworkUp = @"network up";
NSString *const RuneControlCommandNetworkDown = @"network down";
NSString *const RuneControlCommandStop = @"stop";

// The server never answers with more than a line; anything longer is
// not the server.
static const size_t kRuneControlMaxReply = 4096;

static NSError *runeControlError(NSString *message) {
  return [NSError errorWithDomain:RuneControlErrorDomain
                             code:1
                         userInfo:@{NSLocalizedDescriptionKey : message}];
}

static NSError *runeControlErrno(NSString *what) {
  // Captured first: the Objective-C calls below may set errno.
  int err = errno;
  NSString *message =
      [NSString stringWithFormat:@"%@: %s", what, strerror(err)];
  return [NSError errorWithDomain:NSPOSIXErrorDomain
                             code:err
                         userInfo:@{NSLocalizedDescriptionKey : message}];
}

@implementation RuneControl

+ (NSString *)socketPath {
  return [[RuneStatus serversDirectory] stringByAppendingPathComponent:@"rune.ctl.sock"];
}

+ (BOOL)send:(NSString *)command
     timeout:(NSTimeInterval)timeout
       error:(NSError *_Nullable *_Nullable)error {
  NSError *failure = nil;
  BOOL ok = [self sendCommand:command timeout:timeout error:&failure];
  if (!ok && error != NULL) {
    *error = failure;
  }
  return ok;
}

+ (BOOL)sendCommand:(NSString *)command
            timeout:(NSTimeInterval)timeout
              error:(NSError **)error {
  struct sockaddr_un addr;
  memset(&addr, 0, sizeof(addr));
  addr.sun_family = AF_UNIX;
  const char *path = [[self socketPath] fileSystemRepresentation];
  if (strlen(path) >= sizeof(addr.sun_path)) {
    *error = runeControlError(@"control socket path is too long");
    return NO;
  }
  strlcpy(addr.sun_path, path, sizeof(addr.sun_path));

  int fd = socket(AF_UNIX, SOCK_STREAM, 0);
  if (fd < 0) {
    *error = runeControlErrno(@"open socket");
    return NO;
  }
  struct timeval tv;
  tv.tv_sec = (time_t)timeout;
  tv.tv_usec = (suseconds_t)((timeout - (double)tv.tv_sec) * 1e6);
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

  if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
    *error = runeControlErrno(@"no background server is running");
    close(fd);
    return NO;
  }

  NSData *line = [[command stringByAppendingString:@"\n"]
      dataUsingEncoding:NSUTF8StringEncoding];
  const char *bytes = [line bytes];
  size_t remaining = [line length];
  while (remaining > 0) {
    ssize_t n = write(fd, bytes, remaining);
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      *error = runeControlErrno(@"send command");
      close(fd);
      return NO;
    }
    bytes += n;
    remaining -= (size_t)n;
  }
  shutdown(fd, SHUT_WR);

  NSMutableData *reply = [NSMutableData data];
  char buf[256];
  for (;;) {
    ssize_t n = read(fd, buf, sizeof(buf));
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      *error = runeControlErrno(@"read reply");
      close(fd);
      return NO;
    }
    if (n == 0) {
      break;
    }
    [reply appendBytes:buf length:(NSUInteger)n];
    if (memchr(buf, '\n', (size_t)n) != NULL) {
      break;
    }
    if ([reply length] > kRuneControlMaxReply) {
      *error = runeControlError(@"the reply is not from a Rune server");
      close(fd);
      return NO;
    }
  }
  close(fd);

  NSString *text = [[NSString alloc] initWithData:reply encoding:NSUTF8StringEncoding];
  if (text == nil) {
    *error = runeControlError(@"the reply is not from a Rune server");
    return NO;
  }
  NSRange newline = [text rangeOfString:@"\n"];
  if (newline.location != NSNotFound) {
    text = [text substringToIndex:newline.location];
  }
  if ([text isEqualToString:@"ok"]) {
    return YES;
  }
  if ([text hasPrefix:@"err "]) {
    *error = runeControlError([text substringFromIndex:4]);
    return NO;
  }
  if ([text length] == 0) {
    *error = runeControlError(@"the server closed the connection without answering");
    return NO;
  }
  *error = runeControlError([NSString stringWithFormat:@"unexpected reply %@", text]);
  return NO;
}

@end
