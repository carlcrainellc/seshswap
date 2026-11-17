// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {Ed25519} from "../src/Ed25519.sol";

contract EdHarness {
    function mul(uint256 k) external view returns (uint256) { return Ed25519.mulBaseEncoded(k); }
}

contract Ed25519Test is Test {
    struct V { uint256 k; uint256 enc; }
    V[] v;
    EdHarness h;
    uint256 constant L = Ed25519.L;

    function setUp() public {
        h = new EdHarness();
        // independent reference: filippo.io/edwards25519 via swapd/cmd/edvec
        v.push(V(0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5, 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b));
        v.push(V(0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25, 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254));
        v.push(V(0x024cef26f329ad005e0ff56d3939c73e483ec37e64d60f67b6180325a82d1d1d, 0x8425c9d0108b842d2ad640eda406b72398a7c1217768a3a777406fd335ef7045));
        v.push(V(0x056b027e30eff6005290eff0dee84487fd3c6e155cd410b88dc40824b3b31736, 0x86315e32b651bc18cd959a9b6199c18a9bb52a8a217531a56fad22478095ea54));
        v.push(V(0x0f05c54a0520e2f0506d9e53f34a50fc06528942eca04446704a487454841ddd, 0x252debf4652aa6b8751728e615587738621575b86219769ae6798abc99133600));
        v.push(V(0x0436b73f449905e23bd8516e9d164f668aad8ca2207b936d4ed127a384b9a353, 0x97120b11e05b88b8d3b7ba193b676f6dd7b27ecf6f28c5b67cd7bd102efd16c3));
        v.push(V(0x04b0f94494513e165995d25a698d7f5ebce6325a7faeb4adee0c6f927a8fd2a8, 0x22bbe6f0377bd42bdea6b02ddcfeb2e4c78f4a872d5347662f62d044e081c9b5));
        v.push(V(0x0368e60106df81a86c86d436ead33907ecc1b976200eda35b090fd6a85cae5cd, 0x63d68cdef2f51cb1eac2e95dad21a015d8e20f6c1390d20622c992256ecdc393));
        v.push(V(0x05374ed30ebc9541b706cae23e2d86e087a2c1fbb1d383ce9aa008647876772e, 0xc3279bfb4a2f35a98cde5b114b10e67c2699358c76a9f25a7429efef8cc5edf7));
        v.push(V(0x038f705d46114acceb4292f93353acd1e1294384554de5e59f04e1661f863f51, 0x5f2a5731f6fabc9164963158f552ca594c5d1fa0d1454d23f19988c419cd69e3));
        v.push(V(0x08e8c3159d2dc237d81a3b72de01edb80dd406eb041ed88d50f013667106ccbe, 0x5b9ea060001c91ffba2b5d83c08f7fc9d842d552efeacf36c31581299accd074));
        v.push(V(0x0d4ff43868f19c81bedbd2ae5fa249c9222f5b963bcf48ca5cee1967181e39c0, 0x4b08bd00fd81f2631ecae4594dc6de8fb17f491891aab0297267fa614d5327fc));
        v.push(V(0x03ff8bd62a11ac079f87d407f4a62592fc838174973eb18b169e9cb8d9502ace, 0xbafd44b61d827ef4ba2596b4a3c665b20e4c6cee6b8d7c0a5fcea2c54ca37438));
        v.push(V(0x021508ca629bf240e4de9f69675bc190266b88ac6a5b60eaf5068e104a42eec9, 0xc757a4dec1920ec537103d284642f4199b780a4ed7325274174460abe037516a));
        v.push(V(0x0c80ef9cd38e72d13d691add1a03591d5fa10fd77b73b2f30d835b9bc3fc32ee, 0x1114e424a5e5fc560647955bf972c75cee7f82d519d922c7fd9818e65946aea9));
        v.push(V(0x0fab64915b1cb03805ececdaea56159ce864b560b355323879f4f75ffbdd932a, 0x7c0532048b148f7b4d798f75d42753a213f7072a93cb7fd867b5646b0076b4f6));
        v.push(V(0x0dff5e271f421e4f6e6a6c8e9c782bbc35bc0dbf449def6578c9e6da31402b6a, 0x9cb23f1536005579e2e1e1801ecfa0ebf41c62ea962f6ddeab580ffc27071342));
        v.push(V(0x099dba144467b17fabe35cfb276503ec0ca39bcc084d3463a91b3d3751c0054c, 0x7333e732d548f346dd816e4ee843fd7d9196cc19cb380512f97c0a928b73ec8b));
        v.push(V(0x0f81326ddcbfce8f7c7df950b3a4978e772acaf585f9862b8139f209dc8e971b, 0xa958ff9c5bfb04654f1d2b34458975e706b8285f567c3a8f9af843b4585c95f4));
        v.push(V(0x05117cd6ae96cab92066461fbb7a43f618932ed9fe59ec232505ca942f036c3f, 0x32777d1975e6af0f09e81ba023d94c5797724441800cca4317945c29456e6401));
        v.push(V(1, 0x6666666666666666666666666666666666666666666666666666666666666658));
        v.push(V(2, 0x2260cdf3092329c21da25ee8c9a21f5697390f51643851560e5f46ae6af8a3c9));
        v.push(V(L - 1, 0xe666666666666666666666666666666666666666666666666666666666666658));
        v.push(V(1 << 252, 0x5f85adb7749cc735ec838bcccfe7d4b52902c6132998d7ac8e032cad031c42b8));
    }

    function test_KnownVectors() public view {
        for (uint256 i = 0; i < v.length; i++) {
            assertEq(h.mul(v[i].k), v[i].enc, "vector mismatch");
        }
    }

    function test_RejectsZeroAndOutOfRange() public {
        vm.expectRevert(bytes("ed25519: bad scalar"));
        h.mul(0);
        vm.expectRevert(bytes("ed25519: bad scalar"));
        h.mul(L);
        vm.expectRevert(bytes("ed25519: bad scalar"));
        h.mul(type(uint256).max);
    }

    /// -P has the same y and the opposite x-parity: enc(l-k) == enc(k) ^ 2^255.
    function testFuzz_Negation(uint256 k) public view {
        k = bound(k, 1, L - 1);
        assertEq(h.mul(L - k), h.mul(k) ^ (1 << 255));
    }

    /// Differential fuzz against filippo.io/edwards25519 (run: forge test --ffi,
    /// after: (cd ../swapd && go build -o bin/edvec ./cmd/edvec)).
    /// forge-config: default.fuzz.runs = 2000
    function testFuzz_DifferentialFFI(uint256 k) public {
        if (!vm.envOr("ED_FFI", false)) return;
        k = bound(k, 1, L - 1);
        string[] memory cmd = new string[](2);
        cmd[0] = "../swapd/bin/edvec";
        cmd[1] = vm.toString(k);
        uint256 want = abi.decode(vm.ffi(cmd), (uint256));
        assertEq(h.mul(k), want);
    }

    function test_GasMulBase() public {
        uint256 g = gasleft();
        h.mul(v[0].k);
        emit log_named_uint("mulBaseEncoded gas (incl. call)", g - gasleft());
    }

}
