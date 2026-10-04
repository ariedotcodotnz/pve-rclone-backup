# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fake pve-rclone-backupd for tests: serves canned JSON over a unix socket
# from a forked child and logs every request to a file.
package FakeDaemon;

use v5.36;

use File::Temp qw(tempdir);
use IO::Socket::UNIX;
use JSON ();
use POSIX ();
use Socket qw(SOCK_STREAM SOMAXCONN);

my $json = JSON->new->utf8->canonical;

# new(routes => { 'GET /v1/x' => sub ($req) { return (200, $data) } }, delay => secs)
sub new($class, %opts) {
    my $dir = tempdir(CLEANUP => 1);
    my $self = bless {
        dir => $dir,
        socket => "$dir/api.sock",
        log => "$dir/requests.jsonl",
        routes => $opts{routes} // {},
        delay => $opts{delay} // 0,
    }, $class;

    my $server = IO::Socket::UNIX->new(Type => SOCK_STREAM, Local => $self->{socket}, Listen => SOMAXCONN)
        or die "fake daemon: listen: $!";
    my $pid = fork() // die "fork: $!";
    if (!$pid) {
        $self->_serve($server);
        POSIX::_exit(0);
    }
    close($server);
    $self->{pid} = $pid;
    return $self;
}

sub socket_path($self) { return $self->{socket} }

sub requests($self) {
    open(my $fh, '<', $self->{log}) or return [];
    my @r = map { $json->decode($_) } <$fh>;
    return \@r;
}

sub stop($self) {
    return if !$self->{pid};
    kill 'TERM', $self->{pid};
    waitpid($self->{pid}, 0);
    delete $self->{pid};
}

sub DESTROY($self) { $self->stop() }

sub _serve($self, $server) {
    while (my $conn = $server->accept()) {
        my $buf = '';
        while ($buf !~ m/\r\n\r\n/) {
            my $n = sysread($conn, $buf, 65536, length $buf) or last;
        }
        my ($head, $body) = split(/\r\n\r\n/, $buf, 2);
        $body //= '';
        my ($len) = ($head // '') =~ m/^Content-Length:\s*(\d+)/mi;
        while (defined($len) && length($body) < $len) {
            sysread($conn, $body, $len - length($body), length $body) or last;
        }
        my ($method, $target) = ($head // '') =~ m/^(\S+) (\S+) HTTP/;
        my ($path, $query) = split(/\?/, $target // '', 2);
        my %headers = map { m/^([^:]+):\s*(.*)$/ ? (lc($1) => $2) : () } split(/\r\n/, $head // '');
        my $req = {
            method => $method,
            path => $path,
            query => $query // '',
            client_api => $headers{'x-client-api'},
            body => length($body) ? $json->decode($body) : undef,
            raw => $body,
        };
        if (open(my $fh, '>>', $self->{log})) {
            print $fh $json->encode($req), "\n";
            close($fh);
        }

        sleep($self->{delay}) if $self->{delay};

        my $route = $self->{routes}->{"$method $path"};
        my ($status, $data) = $route
            ? $route->($req)
            : (404, { error => { code => 'not_found', message => "no route for $method $path" } });
        my $out = defined($data) ? $json->encode($data) : '';
        my $reason = $status < 400 ? 'OK' : 'Error';
        syswrite($conn, "HTTP/1.1 $status $reason\r\nContent-Type: application/json\r\n"
            . "Content-Length: " . length($out) . "\r\nConnection: close\r\n\r\n$out");
        close($conn);
    }
}

1;
