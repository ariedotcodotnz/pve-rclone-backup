# SPDX-License-Identifier: AGPL-3.0-or-later
use v5.36;

use FindBin;
my $bin;
BEGIN { ($bin) = $FindBin::Bin =~ m/^(.*)$/ } # untaint for perl -T
use lib "$bin/lib";

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

done_testing();
