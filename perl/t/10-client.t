# SPDX-License-Identifier: AGPL-3.0-or-later
use v5.36;

use FindBin;
my $bin;
BEGIN { ($bin) = $FindBin::Bin =~ m/^(.*)$/ } # untaint for perl -T
use lib "$bin/lib";

use File::Temp qw(tempdir);
use POSIX ();
use Socket qw(AF_UNIX SOCK_STREAM pack_sockaddr_un);
use Test::More;
use Time::HiRes ();

use FakeDaemon;
use PVE::Storage::Custom::RcloneBackup::Client;

my $C = 'PVE::Storage::Custom::RcloneBackup::Client';

my $d = FakeDaemon->new(routes => {
    'GET /v1/version' => sub ($req) { return (200, { api => 1, daemon => 'test' }) },
    'POST /v1/echo' => sub ($req) { return (200, { got => $req->{body}, query => $req->{query} }) },
    'GET /v1/fail' => sub ($req) {
        return (409, { error => { code => 'protected', message => 'backup is protected' } });
    },
    'GET /v1/plain-error' => sub ($req) { return (500, undef) },
});
local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $d->socket_path;

is_deeply($C->can('request')->('GET', '/v1/version'), { api => 1, daemon => 'test' }, 'GET decodes JSON');

my $res = $C->can('request')->('POST', '/v1/echo', body => { a => [1, 2], s => "ü" },
    query => { vmid => 100, x => 'a b&c' });
is_deeply($res->{got}, { a => [1, 2], s => "ü" }, 'POST body round-trips (UTF-8)');
is($res->{query}, 'vmid=100&x=a%20b%26c', 'query string is escaped');

eval { $C->can('request')->('GET', '/v1/fail') };
is($@, "rclone-backup: backup is protected\n", 'daemon error message propagated');
is($PVE::Storage::Custom::RcloneBackup::Client::LAST_ERROR_CODE, 'protected', 'error code recorded');

eval { $C->can('request')->('GET', '/v1/plain-error') };
like($@, qr/HTTP 500/, 'error without body reported');

my $reqs = $d->requests;
ok(!grep({ ($_->{client_api} // '') ne '1' } @$reqs), 'every request carries X-Client-API');
ok(!grep({ ($_->{proto} // '') ne 'HTTP/1.0' } @$reqs), 'requests are HTTP/1.0, so responses are never chunked');
$d->stop;

{
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = '/nonexistent/api.sock';
    my $t0 = Time::HiRes::time();
    eval { $C->can('request')->('GET', '/v1/version', timeout => 2) };
    like($@, qr/daemon not reachable/, 'missing daemon reported');
    cmp_ok(Time::HiRes::time() - $t0, '<', 1, 'missing daemon fails fast');
}

{
    my $slow = FakeDaemon->new(delay => 5, routes => {
        'GET /v1/version' => sub ($req) { return (200, {}) },
    });
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $slow->socket_path;
    my $t0 = Time::HiRes::time();
    eval { $C->can('request')->('GET', '/v1/version', timeout => 1) };
    my $elapsed = Time::HiRes::time() - $t0;
    like($@, qr/timeout/, 'hung daemon times out');
    cmp_ok($elapsed, '<', 2, 'timeout honoured');
    $slow->stop;
}

# A response without Content-Length (as Go sends when a handler sets none)
# is read until the daemon closes the connection.
{
    my $stream = FakeDaemon->new(framing => 'stream', routes => {
        'GET /v1/big' => sub ($req) { return (200, { items => [map { { n => $_ } } 1 .. 2000] }) },
    });
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $stream->socket_path;
    my $res = eval { $C->can('request')->('GET', '/v1/big', timeout => 5) };
    is(scalar(@{ $res->{items} // [] }), 2000, 'response without Content-Length decoded') or diag($@);
    $stream->stop;
}

# A daemon that no longer accepts connections (listen backlog full) must not
# block the caller past the deadline in connect().
{
    my $dir = tempdir(CLEANUP => 1);
    my ($path) = "$dir/full.sock" =~ m/^(.*)$/;
    socket(my $listener, AF_UNIX, SOCK_STREAM, 0) or die "socket: $!";
    bind($listener, pack_sockaddr_un($path)) or die "bind: $!";
    listen($listener, 0) or die "listen: $!";
    my @queued;
    for (1 .. 16) {
        socket(my $c, AF_UNIX, SOCK_STREAM, 0) or die "socket: $!";
        $c->blocking(0);
        last if !connect($c, pack_sockaddr_un($path));
        push @queued, $c;
    }
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $path;
    my $t0 = Time::HiRes::time();
    my $ok = eval {
        local $SIG{ALRM} = sub { die "blocked\n" };
        alarm(5);
        eval { $C->can('request')->('GET', '/v1/version', timeout => 0.3) };
        my $err = $@;
        alarm(0);
        die $err if $err eq "blocked\n";
        like($err, qr/timeout connecting to daemon/, 'full backlog reported as a timeout');
        1;
    };
    alarm(0);
    ok($ok, 'connect does not block past the deadline') or diag($@);
    cmp_ok(Time::HiRes::time() - $t0, '<', 1.5, 'connect deadline honoured');
}

# A daemon that drops the connection while a large request is sent must
# not kill the caller with SIGPIPE.
{
    my $drop = FakeDaemon->new(close_early => 1);
    local $PVE::Storage::Custom::RcloneBackup::Client::SOCKET_PATH = $drop->socket_path;
    my $pid = fork() // die "fork: $!";
    if (!$pid) {
        local $SIG{PIPE} = 'DEFAULT';
        eval { $C->can('request')->('PATCH', '/v1/x', body => { notes => 'x' x (8 << 20) }, timeout => 10) };
        POSIX::_exit($@ =~ m/error (?:sending request to|reading) daemon|daemon/ ? 0 : 1);
    }
    waitpid($pid, 0);
    is($? & 127, 0, 'no SIGPIPE when the daemon disconnects mid-request');
    is($? >> 8, 0, 'disconnect reported as an error');
    $drop->stop;
}

done_testing();
