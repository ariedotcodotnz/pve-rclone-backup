# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Minimal client for the pve-rclone-backupd API: HTTP + JSON over the
# daemon's root-only unix socket. Requests are HTTP/1.0, so responses are
# never chunked: they carry a Content-Length or end when the daemon closes
# the connection.
#
# PVE daemons fork workers, so every request opens its own connection and
# nothing is cached across forks. Values returned from the daemon are tainted
# under perl -T; callers must untaint anything used in paths or commands.
package PVE::Storage::Custom::RcloneBackup::Client;

use v5.36;

use IO::Select;
use IO::Socket::UNIX;
use JSON ();
use Socket qw(SOCK_STREAM SOL_SOCKET SO_ERROR pack_sockaddr_un);
use Time::HiRes ();

our $SOCKET_PATH = '/run/pve-rclone-backup/api.sock';

# API revision this client speaks. The daemon rejects incompatible clients
# with error code "incompatible_version".
use constant CLIENT_API => 1;

# Code of the last daemon error, for callers that need to distinguish errors.
our $LAST_ERROR_CODE;

my $json = JSON->new->utf8->canonical;

sub uri_escape($s) {
    $s =~ s/([^A-Za-z0-9\-._~])/sprintf('%%%02X', ord($1))/ge;
    return $s;
}

sub _fail($code, $message) {
    $LAST_ERROR_CODE = $code;
    chomp $message;
    die "rclone-backup: $message\n";
}

sub _remaining($deadline) {
    my $left = $deadline - Time::HiRes::time();
    return $left > 0 ? $left : 0;
}

# Connects without blocking past the deadline: a daemon that stops
# accepting fills its listen backlog, and a blocking connect() would then
# wait indefinitely (Linux answers a non-blocking one with EAGAIN instead).
sub _connect($deadline) {
    my $sock = IO::Socket::UNIX->new(Type => SOCK_STREAM)
        or _fail('unavailable', "cannot create socket: $!");
    $sock->blocking(0);
    my $addr = pack_sockaddr_un($SOCKET_PATH);
    while (!connect($sock, $addr)) {
        if ($!{EAGAIN} || $!{EINTR}) {
            _remaining($deadline) > 0
                or _fail('timeout', "timeout connecting to daemon at $SOCKET_PATH");
            Time::HiRes::sleep(0.01);
            next;
        }
        if ($!{EINPROGRESS}) {
            IO::Select->new($sock)->can_write(_remaining($deadline))
                or _fail('timeout', "timeout connecting to daemon at $SOCKET_PATH");
            $! = unpack('i', getsockopt($sock, SOL_SOCKET, SO_ERROR) // pack('i', 0));
            last if !$!;
        }
        _fail('unavailable', "daemon not reachable at $SOCKET_PATH ($!); is pve-rclone-backupd running?");
    }
    return $sock;
}

# request($method, $path, %opts) -> decoded JSON (hash/array) or undef
#
# Options: query => {..}, body => {..}, timeout => seconds (default 10).
sub request($method, $path, %opts) {
    $LAST_ERROR_CODE = undef;
    my $timeout = $opts{timeout} // 10;
    my $deadline = Time::HiRes::time() + $timeout;
    # A daemon that goes away mid-request must not kill the calling PVE
    # worker with SIGPIPE; the write fails with EPIPE instead.
    local $SIG{PIPE} = 'IGNORE';

    if (my $query = $opts{query}) {
        my @pairs = map { uri_escape($_) . '=' . uri_escape($query->{$_}) }
            grep { defined $query->{$_} } sort keys %$query;
        $path .= '?' . join('&', @pairs) if @pairs;
    }

    my $body = defined($opts{body}) ? $json->encode($opts{body}) : '';
    my $req = "$method $path HTTP/1.0\r\n"
        . "Host: pve-rclone-backupd\r\n"
        . "Connection: close\r\n"
        . "Accept: application/json\r\n"
        . "X-Client-API: " . CLIENT_API . "\r\n";
    $req .= "Content-Type: application/json\r\n" if length $body;
    $req .= "Content-Length: " . length($body) . "\r\n\r\n" . $body;

    my $sock = _connect($deadline);
    my $sel = IO::Select->new($sock);

    my $off = 0;
    while ($off < length $req) {
        $sel->can_write(_remaining($deadline))
            or _fail('timeout', "timeout sending request to daemon");
        my $n = syswrite($sock, $req, length($req) - $off, $off);
        next if !defined($n) && $!{EAGAIN};
        defined($n) or _fail('io', "error sending request to daemon: $!");
        $off += $n;
    }

    my $resp = '';
    while (1) {
        $sel->can_read(_remaining($deadline))
            or _fail('timeout', "timeout waiting for daemon response");
        my $n = sysread($sock, $resp, 65536, length $resp);
        next if !defined($n) && $!{EAGAIN};
        defined($n) or _fail('io', "error reading daemon response: $!");
        last if $n == 0;
        last if _complete(\$resp);
    }
    close($sock);

    my ($head, $content) = split(/\r\n\r\n/, $resp, 2);
    my ($status) = ($head // '') =~ m!^HTTP/1\.[01] (\d{3})!
        or _fail('protocol', "malformed response from daemon");
    $content //= '';

    my $data;
    if (length $content) {
        $data = eval { $json->decode($content) };
        _fail('protocol', "invalid JSON from daemon: $@") if $@;
    }
    if ($status >= 400) {
        my $err = ref($data) eq 'HASH' ? $data->{error} : undef;
        _fail($err->{code} // "http_$status", $err->{message} // "daemon returned HTTP $status")
            if ref($err) eq 'HASH';
        _fail("http_$status", "daemon returned HTTP $status");
    }
    return $data;
}

# A response is complete once all Content-Length bytes have arrived.
sub _complete($ref) {
    my $end = index($$ref, "\r\n\r\n");
    return 0 if $end < 0;
    my $head = substr($$ref, 0, $end);
    my ($len) = $head =~ m/^Content-Length:\s*(\d+)\s*$/mi;
    return 0 if !defined $len;
    return length($$ref) - $end - 4 >= $len;
}

1;
